package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"feedsystem_video_go/internal/config"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Storage struct {
	core   *minio.Core
	signer *minio.Client
	bucket string
}

func OpenStorage(cfg config.MediaConfig) (*Storage, error) {
	if cfg.Endpoint == "" {
		return nil, nil
	}
	if cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("media storage requires bucket, access key and secret key")
	}
	host, secure, err := endpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	opts := minio.Options{Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure, Region: "us-east-1", BucketLookup: minio.BucketLookupPath, MaxRetries: 2}
	core, err := minio.NewCore(host, &opts)
	if err != nil {
		return nil, err
	}
	if cfg.PublicEndpoint != "" {
		host, opts.Secure, err = endpoint(cfg.PublicEndpoint)
		if err != nil {
			return nil, err
		}
	}
	signer, err := minio.New(host, &opts)
	if err != nil {
		return nil, err
	}
	return &Storage{core: core, signer: signer, bucket: cfg.Bucket}, nil
}

func endpoint(raw string) (string, bool, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", false, errors.New("media storage endpoint must be an HTTP(S) origin")
	}
	return u.Host, u.Scheme == "https", nil
}

func (s *Storage) EnsureBucket(ctx context.Context) error {
	exists, err := s.core.BucketExists(ctx, s.bucket)
	if err != nil || exists {
		return err
	}
	err = s.core.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: "us-east-1"})
	if minio.ToErrorResponse(err).Code == "BucketAlreadyOwnedByYou" {
		return nil
	}
	return err
}

func (s *Storage) start(ctx context.Context, a Asset) (string, error) {
	return s.core.NewMultipartUpload(ctx, s.bucket, a.source(), minio.PutObjectOptions{ContentType: "application/octet-stream"})
}

func (s *Storage) partURL(ctx context.Context, a Asset, number int) (string, error) {
	params := url.Values{"uploadId": {a.UploadID}, "partNumber": {strconv.Itoa(number)}}
	// The browser supplies Content-Length from the Blob. Signing it prevents
	// a URL intended for one part from accepting a larger body.
	head := http.Header{"Content-Length": {strconv.FormatInt(a.partSize(number), 10)}}
	ttl := min(15*time.Minute, time.Until(time.UnixMilli(a.ExpiresAtMillis)))
	if ttl < time.Second {
		return "", ErrExpired
	}
	u, err := s.signer.PresignHeader(ctx, http.MethodPut, s.bucket, a.source(), ttl, params, head)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s *Storage) parts(ctx context.Context, a Asset) ([]minio.ObjectPart, error) {
	var parts []minio.ObjectPart
	for marker := 0; ; {
		page, err := s.core.ListObjectParts(ctx, s.bucket, a.source(), a.UploadID, marker, 100)
		if err != nil {
			return nil, err
		}
		parts = append(parts, page.ObjectParts...)
		if !page.IsTruncated {
			return parts, nil
		}
		if page.NextPartNumberMarker <= marker {
			return nil, errors.New("media storage part listing did not advance")
		}
		marker = page.NextPartNumberMarker
	}
}

func (s *Storage) complete(ctx context.Context, a Asset, parts []minio.CompletePart) error {
	_, err := s.core.CompleteMultipartUpload(ctx, s.bucket, a.source(), a.UploadID, parts, minio.PutObjectOptions{})
	return err
}

func (s *Storage) sourceExists(ctx context.Context, a Asset) (bool, error) {
	info, err := s.core.StatObject(ctx, s.bucket, a.source(), minio.StatObjectOptions{})
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NotFound":
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Size != a.Size {
		return false, ErrIncomplete
	}
	return true, nil
}

func (s *Storage) openSource(ctx context.Context, a Asset) (io.ReadCloser, error) {
	r, _, _, err := s.core.GetObject(ctx, s.bucket, a.source(), minio.GetObjectOptions{})
	return r, err
}

func (s *Storage) uploadOutput(ctx context.Context, key, path, contentType string) error {
	_, err := s.core.FPutObject(ctx, s.bucket, key, path, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *Storage) playbackURL(ctx context.Context, key, method string) (string, error) {
	u, err := s.signer.Presign(ctx, method, s.bucket, key, 15*time.Minute, nil)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s *Storage) removeObject(ctx context.Context, key string) error {
	return s.core.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *Storage) removePrefix(ctx context.Context, prefix, keep string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for obj := range s.core.Client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if keep != "" && strings.HasPrefix(obj.Key, keep) {
			continue
		}
		if err := s.removeObject(ctx, obj.Key); err != nil {
			return fmt.Errorf("remove media object: %w", err)
		}
	}
	return ctx.Err()
}

func (s *Storage) discard(ctx context.Context, a Asset) error {
	if a.UploadID != "" {
		if err := s.core.AbortMultipartUpload(ctx, s.bucket, a.source(), a.UploadID); err != nil &&
			minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			return err
		}
	}
	// Also remove abandoned multipart uploads whose ID was never committed.
	if err := s.abortAbandoned(ctx, a); err != nil {
		return err
	}
	return s.removePrefix(ctx, a.prefix(), "")
}

func (s *Storage) abortAbandoned(ctx context.Context, a Asset) error {
	err := s.core.RemoveIncompleteUpload(ctx, s.bucket, a.source())
	if minio.ToErrorResponse(err).Code == "NoSuchUpload" {
		return nil
	}
	return err
}

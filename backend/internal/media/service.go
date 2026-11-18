package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	db      *gorm.DB
	storage *Storage
}

func NewService(db *gorm.DB, storage *Storage) *Service {
	return &Service{db: db, storage: storage}
}

type InitRequest struct {
	DraftID string `json:"draft_id"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

type View struct {
	ID               string    `json:"asset_id"`
	Status           Status    `json:"status"`
	PartSize         int64     `json:"part_size"`
	PartCount        int       `json:"part_count"`
	Uploaded         []int     `json:"uploaded_parts"`
	ExpiresAt        time.Time `json:"expires_at"`
	Failure          string    `json:"failure,omitempty"`
	DurationMillis   int64     `json:"duration_ms,omitempty"`
	Width            int       `json:"width,omitempty"`
	Height           int       `json:"height,omitempty"`
	PublishedVideoID *uint     `json:"video_id,omitempty"`
}

func (a Asset) view() View {
	status := a.Status
	if a.ExpiresAtMillis <= time.Now().UnixMilli() && expires(status) {
		status = Expired
	}
	return View{
		ID: a.ID, Status: status, PartSize: PartSize, PartCount: a.PartCount(), Uploaded: []int{},
		ExpiresAt: time.UnixMilli(a.ExpiresAtMillis).UTC(), Failure: a.Failure, PublishedVideoID: a.PublishedVideoID,
		DurationMillis: a.DurationMillis, Width: a.Width, Height: a.Height,
	}
}

func expires(status Status) bool {
	return status == Uploading || status == Ready || status == Failed
}

func LoadOwned(db *gorm.DB, owner uint, id string, lock bool) (Asset, error) {
	var a Asset
	if !validHex(id, 32) {
		return a, ErrInvalid
	}
	if lock {
		db = db.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := db.Where("id = ? AND owner_id = ?", id, owner).Take(&a).Error
	return a, err
}

func (s *Service) Init(ctx context.Context, owner uint, in InitRequest) (View, error) {
	if owner == 0 || !validHex(in.DraftID, 32) || !validHex(in.SHA256, 64) || in.Size <= 0 || in.Size > MaxUploadBytes {
		return View{}, ErrInvalid
	}
	id, err := newID()
	if err != nil {
		return View{}, err
	}
	draft := Asset{ID: id, OwnerID: owner, DraftID: in.DraftID, Status: Uploading,
		Size: in.Size, SHA256: in.SHA256, ExpiresAtMillis: time.Now().Add(uploadLifetime).UnixMilli()}
	db := s.db.WithContext(ctx)
	// Commit the draft first so cleanup can find a multipart upload even if
	// initiation succeeded just before the transaction below was interrupted.
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&draft).Error; err != nil {
		return View{}, err
	}
	var a Asset
	err = db.Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("owner_id = ? AND draft_id = ?", owner, in.DraftID).Take(&a).Error
		if err != nil {
			return err
		}
		if a.Size != in.Size || a.SHA256 != in.SHA256 {
			return ErrConflict
		}
		if a.view().Status != Uploading || a.UploadID != "" {
			return nil
		}
		if err := s.storage.abortAbandoned(ctx, a); err != nil {
			return storageError(err)
		}
		a.UploadID, err = s.storage.start(ctx, a)
		if err != nil {
			return storageError(err)
		}
		return tx.Model(&a).Update("upload_id", a.UploadID).Error
	})
	if err != nil {
		return View{}, err
	}
	return s.describe(ctx, a, true)
}

func (s *Service) Status(ctx context.Context, owner uint, id string) (View, error) {
	a, err := LoadOwned(s.db.WithContext(ctx), owner, id, false)
	if err != nil {
		return View{}, err
	}
	return s.describe(ctx, a, false)
}

func (s *Service) describe(ctx context.Context, a Asset, withParts bool) (View, error) {
	view := a.view()
	if !withParts || view.Status != Uploading || a.UploadID == "" {
		return view, nil
	}
	parts, err := s.storage.parts(ctx, a)
	if len(parts) == 0 && (err == nil || minio.ToErrorResponse(err).Code == "NoSuchUpload") {
		// Some S3 gateways return an empty list after completion. A HEAD
		// distinguishes that case from a newly opened multipart session.
		done, headErr := s.storage.sourceExists(ctx, a)
		if headErr != nil {
			return View{}, storageError(headErr)
		}
		if done {
			for number := 1; number <= a.PartCount(); number++ {
				view.Uploaded = append(view.Uploaded, number)
			}
			return view, nil
		}
	}
	if err != nil {
		return View{}, storageError(err)
	}
	for _, p := range parts {
		if p.Size == a.partSize(p.PartNumber) && p.Size > 0 {
			view.Uploaded = append(view.Uploaded, p.PartNumber)
		}
	}
	return view, nil
}

func (s *Service) PartURL(ctx context.Context, owner uint, id string, number int) (string, error) {
	a, err := LoadOwned(s.db.WithContext(ctx), owner, id, false)
	if err != nil {
		return "", err
	}
	switch a.view().Status {
	case Expired:
		return "", ErrExpired
	case Uploading:
		if a.UploadID == "" {
			return "", ErrConflict
		}
	default:
		return "", ErrConflict
	}
	if a.partSize(number) == 0 {
		return "", ErrInvalid
	}
	u, err := s.storage.partURL(ctx, a, number)
	return u, storageError(err)
}

func (s *Service) Complete(ctx context.Context, owner uint, id string) (View, error) {
	var a Asset
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		a, err = LoadOwned(tx, owner, id, true)
		if err != nil {
			return err
		}
		if a.view().Status == Expired {
			return ErrExpired
		}
		if a.Status != Uploading {
			return nil
		}
		if a.UploadID == "" {
			return ErrConflict
		}
		if err := s.completeObject(ctx, a); err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		a.Status, a.DispatchAtMillis, a.RetryAtMillis = Queued, &now, &now
		return tx.Model(&a).Updates(map[string]any{
			"status": Queued, "dispatch_at_millis": now, "retry_at_millis": now,
		}).Error
	})
	return a.view(), err
}

func (s *Service) completeObject(ctx context.Context, a Asset) error {
	done, err := s.storage.sourceExists(ctx, a)
	if err != nil || done {
		return storageError(err)
	}
	parts, err := s.storage.parts(ctx, a)
	if err != nil {
		return storageError(err)
	}
	pending := make([]minio.CompletePart, a.PartCount())
	if len(parts) != len(pending) {
		return ErrIncomplete
	}
	for _, part := range parts {
		n := part.PartNumber
		if a.partSize(n) == 0 || a.partSize(n) != part.Size || strings.Trim(part.ETag, "\"") == "" || pending[n-1].PartNumber != 0 {
			return ErrIncomplete
		}
		pending[n-1] = minio.CompletePart{PartNumber: n, ETag: part.ETag}
	}
	if err := s.storage.complete(ctx, a, pending); err != nil {
		// If the completion response was lost, a later retry finds the object.
		switch minio.ToErrorResponse(err).Code {
		case "InvalidPart", "EntityTooSmall":
			return ErrIncomplete
		default:
			return storageError(err)
		}
	}
	done, err = s.storage.sourceExists(ctx, a)
	if err != nil {
		return storageError(err)
	}
	if !done {
		return ErrUnavailable
	}
	return nil
}

func (s *Service) Cancel(ctx context.Context, owner uint, id string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		a, err := LoadOwned(tx, owner, id, true)
		if err != nil || a.Status == Expired {
			return err
		}
		if a.Status != Uploading && a.Status != Ready && a.Status != Failed {
			return ErrConflict
		}
		return tx.Model(&a).Update("status", Expired).Error
	})
}

func storageError(err error) error {
	if err == nil || errors.Is(err, ErrIncomplete) || errors.Is(err, ErrExpired) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

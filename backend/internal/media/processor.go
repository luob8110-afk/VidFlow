package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	videoOutput = "play.mp4"
	coverOutput = "cover.jpg"
	maxDuration = 5 * 60
)

var (
	errChecksum = errors.New("视频校验失败，请重新选择文件")
	errMedia    = errors.New("视频格式不受支持、文件损坏或超过处理限制")
)

type probeInfo struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

type result struct {
	DurationMillis int64
	Width          int
	Height         int
}

func (s *Service) process(ctx context.Context, a Asset) (result, error) {
	dir, err := os.MkdirTemp("", "vidflow-media-")
	if err != nil {
		return result{}, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "source")
	if err := s.downloadVerified(ctx, a, input); err != nil {
		return result{}, err
	}
	sourceInfo, err := probe(ctx, input)
	if err != nil {
		return result{}, err
	}
	video := filepath.Join(dir, videoOutput)
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-xerror",
		"-max_alloc", "268435456", "-threads", "2", "-filter_threads", "1",
		"-protocol_whitelist", "file", "-format_whitelist", "mov,matroska,webm",
		"-i", input, "-map", "0:V:0", "-map", "0:a:0?", "-map_metadata", "-1", "-map_chapters", "-1",
		"-vf", "scale=w=min(iw\\,1280):h=min(ih\\,1280):force_original_aspect_ratio=decrease:force_divisible_by=2,setsar=1",
		"-c:v", "libx264", "-threads", "2", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-ac", "2", "-b:a", "128k", "-movflags", "+faststart",
		"-t", strconv.Itoa(maxDuration), "-fs", strconv.FormatInt(MaxUploadBytes, 10), video}
	if err := runFFmpeg(ctx, args...); err != nil {
		return result{}, err
	}
	info, err := probe(ctx, video)
	if err != nil {
		return result{}, err
	}
	// FFmpeg can return success when the output file-size limit stops encoding.
	// Reject that result instead of publishing only a prefix of the clip.
	if info.DurationMillis+500 < sourceInfo.DurationMillis {
		return result{}, errMedia
	}
	cover := filepath.Join(dir, coverOutput)
	if err := runFFmpeg(ctx, "-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "1",
		"-protocol_whitelist", "file", "-f", "mov", "-i", video, "-map", "0:V:0", "-frames:v", "1", "-threads", "1", "-q:v", "3", cover); err != nil {
		return result{}, err
	}
	if err := s.storage.uploadOutput(ctx, a.output(a.LeaseToken, videoOutput), video, "video/mp4"); err != nil {
		return result{}, err
	}
	if err := s.storage.uploadOutput(ctx, a.output(a.LeaseToken, coverOutput), cover, "image/jpeg"); err != nil {
		return result{}, err
	}
	return info, nil
}

func (s *Service) downloadVerified(ctx context.Context, a Asset, path string) error {
	source, err := s.storage.openSource(ctx, a)
	if err != nil {
		return err
	}
	defer source.Close()
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, a.Size+1))
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if size != a.Size || hex.EncodeToString(hash.Sum(nil)) != a.SHA256 {
		return errChecksum
	}
	return nil
}

func probe(ctx context.Context, input string) (result, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-max_alloc", "268435456",
		"-protocol_whitelist", "file", "-format_whitelist", "mov,matroska,webm", "-select_streams", "V:0",
		"-show_entries", "stream=width,height:format=duration", "-of", "json", input)
	var stderr limitedLog
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return result{}, mediaCommandError(ctx, err, &stderr)
	}
	var info probeInfo
	if err := json.Unmarshal(out, &info); err != nil || len(info.Streams) != 1 {
		return result{}, errMedia
	}
	stream := info.Streams[0]
	duration, err := strconv.ParseFloat(info.Format.Duration, 64)
	if err != nil || math.IsNaN(duration) || duration <= 0 || duration > maxDuration+0.01 ||
		stream.Width < 2 || stream.Height < 2 || stream.Width > 4096 || stream.Height > 4096 ||
		stream.Width*stream.Height > 9_000_000 {
		return result{}, errMedia
	}
	return result{int64(math.Round(duration * 1000)), stream.Width, stream.Height}, nil
}

func runFFmpeg(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr limitedLog
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return mediaCommandError(ctx, err, &stderr)
	}
	return nil
}

func mediaCommandError(ctx context.Context, err error, stderr *limitedLog) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("%w: %s", errMedia, strings.TrimSpace(stderr.String()))
	}
	return err
}

type limitedLog struct{ bytes.Buffer }

func (l *limitedLog) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 4096 - l.Len()
	if remaining > 0 {
		_, _ = l.Buffer.Write(p[:min(remaining, n)])
	}
	return n, nil
}

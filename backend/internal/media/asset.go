package media

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

type Status string

const (
	Uploading  Status = "uploading"
	Queued     Status = "queued"
	Processing Status = "processing"
	Ready      Status = "ready"
	Failed     Status = "failed"
	Published  Status = "published"
	Expired    Status = "expired"

	PartSize       int64 = 5 << 20
	MaxUploadBytes int64 = 200 << 20
	uploadLifetime       = 24 * time.Hour
	draftLifetime        = 7 * 24 * time.Hour
)

var (
	ErrUnavailable = errors.New("媒体存储暂不可用")
	ErrInvalid     = errors.New("上传参数无效")
	ErrIncomplete  = errors.New("分片未上传完整或大小不匹配")
	ErrConflict    = errors.New("当前媒体状态不允许此操作")
	ErrExpired     = errors.New("上传会话已过期")
)

// The queued row is also a durable dispatch record. A broker message only
// carries its ID, so a process crash cannot lose the processing intent.
type Asset struct {
	ID               string `gorm:"type:char(32);primaryKey"`
	OwnerID          uint   `gorm:"uniqueIndex:idx_media_owner_draft,priority:1;not null"`
	DraftID          string `gorm:"type:char(32);uniqueIndex:idx_media_owner_draft,priority:2;not null"`
	Status           Status `gorm:"type:varchar(16);index:idx_media_dispatch,priority:1;index:idx_media_expiry,priority:1;not null"`
	Size             int64
	SHA256           string `gorm:"type:char(64);not null"`
	UploadID         string `gorm:"type:varchar(512)"`
	ExpiresAtMillis  int64  `gorm:"index:idx_media_expiry,priority:2"`
	DispatchAtMillis *int64 `gorm:"index:idx_media_dispatch,priority:2"`
	RetryAtMillis    *int64
	LeaseUntilMillis *int64
	LeaseToken       string `gorm:"type:char(32)"`
	Attempts         int
	Failure          string `gorm:"type:varchar(255)"`
	DurationMillis   int64
	Width            int
	Height           int
	OutputToken      string `gorm:"type:char(32)"`
	PublishedVideoID *uint  `gorm:"uniqueIndex"`
	CleanedAt        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (a Asset) prefix() string { return "assets/" + a.ID + "/" }
func (a Asset) source() string { return a.prefix() + "source" }
func (a Asset) output(token, name string) string {
	return a.prefix() + "outputs/" + token + "/" + name
}

func (a Asset) PartCount() int { return int((a.Size + PartSize - 1) / PartSize) }

func (a Asset) partSize(number int) int64 {
	if number < 1 || number > a.PartCount() {
		return 0
	}
	if number == a.PartCount() {
		return a.Size - PartSize*int64(number-1)
	}
	return PartSize
}

func validHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func newID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func PlaybackPath(id string, name string) string { return "/media/" + id + "/" + name }

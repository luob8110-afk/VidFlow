package video

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	rediscache "feedsystem_video_go/internal/middleware/redis"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Written in the same transaction as the relationship. Flushed events stay
// available for cache reconstruction and delayed post-commit cache updates.
type LikeCountEvent struct {
	ID        uint64 `gorm:"primaryKey"`
	VideoID   uint   `gorm:"not null;index"`
	Delta     int64  `gorm:"not null"`
	CreatedAt time.Time
	FlushedAt *time.Time `gorm:"index"`
}

// Only batch flushers contend on this row; requests never lock a video counter.
type LikeCountFlushLock struct {
	ID uint `gorm:"primaryKey"`
}

type LikeCountStore struct {
	db    *gorm.DB
	cache *rediscache.Client
}

func NewLikeCountStore(db *gorm.DB, cache *rediscache.Client) *LikeCountStore {
	return &LikeCountStore{db: db, cache: cache}
}

func (s *LikeCountStore) Apply(ctx context.Context, events []LikeCountEvent) error {
	if s.cache == nil || len(events) == 0 {
		return nil
	}
	byVideo := make(map[uint][]LikeCountEvent)
	for _, e := range events {
		byVideo[e.VideoID] = append(byVideo[e.VideoID], e)
	}
	ids := make([]uint, 0, len(byVideo))
	for id := range byVideo {
		ids = append(ids, id)
	}
	existing, err := s.cache.LikeCounts(ctx, ids)
	if err != nil {
		return err
	}
	for id, group := range byVideo {
		var baseline int64
		var included []uint64
		if _, ok := existing[id]; !ok {
			included = make([]uint64, 0)
			// A consistent snapshot prevents a flush from being counted in both
			// videos.likes_count and the outstanding event deltas.
			err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				var v Video
				if err := tx.Select("likes_count").First(&v, id).Error; err != nil {
					return err
				}
				baseline = v.LikesCount
				var journal []LikeCountEvent
				if err := tx.Where("video_id = ?", id).Find(&journal).Error; err != nil {
					return err
				}
				for _, e := range journal {
					included = append(included, e.ID)
					if e.FlushedAt == nil {
						baseline += e.Delta
					}
				}
				return nil
			}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			if err != nil {
				return err
			}
		}
		for _, e := range group {
			if err := s.cache.ApplyLikeCount(ctx, id, baseline, included, e.ID, e.Delta); err != nil {
				return err
			}
		}
	}
	return nil
}

// Flush coalesces up to limit relationship changes into one UPDATE per video.
// Applying Redis before committing the batch makes retries safe across crashes.
func (s *LikeCountStore) Flush(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	processed := 0
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lock := LikeCountFlushLock{ID: 1}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&lock).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lock, 1).Error; err != nil {
			return err
		}
		var events []LikeCountEvent
		if err := tx.Where("flushed_at IS NULL").Order("id").Limit(limit).Find(&events).Error; err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		if err := s.Apply(ctx, events); err != nil {
			return err
		}
		deltas := make(map[uint]int64)
		ids := make([]uint64, 0, len(events))
		for _, e := range events {
			deltas[e.VideoID] += e.Delta
			ids = append(ids, e.ID)
		}
		videos := make([]uint, 0, len(deltas))
		for id := range deltas {
			videos = append(videos, id)
		}
		sort.Slice(videos, func(i, j int) bool { return videos[i] < videos[j] })
		for _, id := range videos {
			if deltas[id] == 0 {
				continue
			}
			if err := tx.Model(&Video{}).Where("id = ?", id).Updates(map[string]interface{}{
				"likes_count": gorm.Expr("likes_count + ?", deltas[id]),
				"popularity":  gorm.Expr("popularity + ?", deltas[id]),
			}).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&LikeCountEvent{}).Where("id IN ?", ids).Update("flushed_at", time.Now()).Error; err != nil {
			return err
		}
		processed = len(events)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return processed, nil
}

func (s *LikeCountStore) Cleanup(ctx context.Context) error {
	// API cache attempts have a bounded lifetime; completed events outlive the
	// entire Redis generation by 24x. Pending events are never expired.
	return s.db.WithContext(ctx).Where("flushed_at < ?", time.Now().Add(-24*time.Hour)).Delete(&LikeCountEvent{}).Error
}

// Counts reads Redis first. Cold keys and Redis outages fall back to a single
// consistent database snapshot including deltas not yet flushed.
func (s *LikeCountStore) Counts(ctx context.Context, ids []uint) (map[uint]int64, error) {
	cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	counts, err := s.cache.LikeCounts(cacheCtx, ids)
	cancel()
	if err != nil {
		counts = make(map[uint]int64)
	}
	var missing []uint
	for _, id := range ids {
		if _, ok := counts[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return counts, nil
	}
	included := make(map[uint][]uint64)
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var videos []Video
		if err := tx.Select("id", "likes_count").Where("id IN ?", missing).Find(&videos).Error; err != nil {
			return err
		}
		for _, v := range videos {
			counts[v.ID] = v.LikesCount
			included[v.ID] = make([]uint64, 0)
		}
		var journal []LikeCountEvent
		if err := tx.Where("video_id IN ?", missing).Find(&journal).Error; err != nil {
			return err
		}
		for _, e := range journal {
			included[e.VideoID] = append(included[e.VideoID], e.ID)
			if e.FlushedAt == nil {
				counts[e.VideoID] += e.Delta
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err == nil && s.cache != nil {
		// Hydrate once on a cold read so cached Feed pages stay database-free.
		for id, markers := range included {
			if cacheErr := s.cache.ApplyLikeCount(ctx, id, counts[id], markers, 0, 0); cacheErr != nil {
				return counts, nil
			}
		}
		if live, cacheErr := s.cache.LikeCounts(ctx, missing); cacheErr == nil {
			for id, n := range live {
				counts[id] = n
			}
		}
	}
	return counts, err
}

func OverlayLikeCounts(ctx context.Context, db *gorm.DB, cache *rediscache.Client, videos []*Video) {
	if len(videos) == 0 {
		return
	}
	ids := make([]uint, len(videos))
	for i, v := range videos {
		ids[i] = v.ID
	}
	opCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	counts, err := NewLikeCountStore(db, cache).Counts(opCtx, ids)
	if err != nil {
		return
	}
	for _, v := range videos {
		if n, ok := counts[v.ID]; ok {
			v.LikesCount = n
		}
	}
}

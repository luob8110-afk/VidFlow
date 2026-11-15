package video

import (
	"context"
	"errors"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"log"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

type LikeService struct {
	repo         *LikeRepository
	VideoRepo    *VideoRepository
	cache        *rediscache.Client
	likeMQ       *rabbitmq.LikeMQ
	popularityMQ *rabbitmq.PopularityMQ
}

func NewLikeService(repo *LikeRepository, videoRepo *VideoRepository, cache *rediscache.Client, likeMQ *rabbitmq.LikeMQ, popularityMQ *rabbitmq.PopularityMQ) *LikeService {
	return &LikeService{repo: repo, VideoRepo: videoRepo, cache: cache, likeMQ: likeMQ, popularityMQ: popularityMQ}
}

func isDupKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func (s *LikeService) Like(ctx context.Context, like *Like) error {
	return s.change(ctx, like, true)
}

func (s *LikeService) Unlike(ctx context.Context, like *Like) error {
	return s.change(ctx, like, false)
}

func (s *LikeService) change(ctx context.Context, like *Like, desired bool) error {
	if like == nil || like.VideoID == 0 || like.AccountID == 0 {
		return errors.New("video_id and account_id are required")
	}
	var event LikeCountEvent
	err := s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var v Video
		if err := tx.Select("id").First(&v, like.VideoID).Error; err != nil {
			return err
		}
		var changed bool
		if desired {
			like.CreatedAt = time.Now()
			err := tx.Create(like).Error
			if isDupKey(err) {
				return nil
			}
			if err != nil {
				return err
			}
			changed = true
			event.Delta = 1
		} else {
			result := tx.Where("video_id = ? AND account_id = ?", like.VideoID, like.AccountID).Delete(&Like{})
			if result.Error != nil {
				return result.Error
			}
			changed = result.RowsAffected > 0
			event.Delta = -1
		}
		if !changed {
			return nil
		}
		event.VideoID = like.VideoID
		return tx.Create(&event).Error
	})
	if err != nil || event.ID == 0 {
		return err
	}

	// The committed journal is the retry source if Redis is unavailable or
	// the request disconnects. Never undo a committed relationship on cache errors.
	cacheCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := NewLikeCountStore(s.repo.db, s.cache).Apply(cacheCtx, []LikeCountEvent{event}); err != nil {
		log.Printf("like counter deferred: event=%d err=%v", event.ID, err)
	}
	if s.likeMQ != nil {
		if err := s.likeMQ.Notify(cacheCtx, like.AccountID, like.VideoID, desired); err != nil {
			log.Printf("like notification publish failed: event=%d err=%v", event.ID, err)
		}
	}
	if s.popularityMQ != nil {
		if err := s.popularityMQ.Update(cacheCtx, like.VideoID, event.Delta); err != nil {
			UpdatePopularityCache(cacheCtx, s.cache, like.VideoID, event.Delta)
		}
	} else {
		UpdatePopularityCache(cacheCtx, s.cache, like.VideoID, event.Delta)
	}
	return nil
}

func (s *LikeService) IsLiked(ctx context.Context, videoID, accountID uint) (bool, error) {
	return s.repo.IsLiked(ctx, videoID, accountID)
}

func (s *LikeService) ListLikedVideos(ctx context.Context, accountID uint) ([]Video, error) {
	videos, err := s.repo.ListLikedVideos(ctx, accountID)
	ptrs := make([]*Video, len(videos))
	for i := range videos {
		ptrs[i] = &videos[i]
	}
	if err == nil {
		OverlayLikeCounts(ctx, s.repo.db, s.cache, ptrs)
	}
	return videos, err
}

package video

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rediscache "feedsystem_video_go/internal/middleware/redis"
	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func countTestStore(t *testing.T) (*LikeCountStore, *LikeService, *miniredis.Miniredis) {
	t.Helper()
	dsn := os.Getenv("COUNT_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("COUNT_TEST_MYSQL_DSN is unset (use an isolated database)")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("count_%d_", time.Now().UnixNano())}})
	if err != nil {
		t.Fatal(err)
	}
	models := []interface{}{&Video{}, &Like{}, &LikeCountEvent{}, &LikeCountFlushLock{}}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	cache := rediscache.NewClient(goredis.NewClient(&goredis.Options{Addr: mr.Addr(), MaxRetries: -1}), "test:")
	t.Cleanup(func() { db.Migrator().DropTable(models...); pool, _ := db.DB(); pool.Close(); cache.Close() })
	v := Video{ID: 1, AuthorID: 99, Username: "author", Title: "video", PlayURL: "/video.mp4", CoverURL: "/cover.jpg"}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	return NewLikeCountStore(db, cache), NewLikeService(NewLikeRepository(db), NewVideoRepository(db), cache, nil, nil), mr
}

func assertCounts(t *testing.T, s *LikeCountStore, cached, persisted int64) {
	t.Helper()
	counts, err := s.Counts(context.Background(), []uint{1})
	if err != nil || counts[1] != cached {
		t.Fatalf("visible counts=%v err=%v want=%d", counts, err, cached)
	}
	var v Video
	if err := s.db.First(&v, 1).Error; err != nil {
		t.Fatal(err)
	}
	if v.LikesCount != persisted {
		t.Fatalf("DB count=%d want=%d", v.LikesCount, persisted)
	}
	// Detail entities may be cached with old database counters; the response
	// must overlay the live counter rather than cache it into the entity.
	detail, err := NewVideoService(NewVideoRepository(s.db), s.cache, nil).GetDetail(context.Background(), 1)
	if err != nil || detail.LikesCount != cached {
		t.Fatalf("detail=%v err=%v want=%d", detail, err, cached)
	}
}

func TestLikeCountCacheFirstBatchAndRecovery(t *testing.T) {
	s, service, mr := countTestStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for user := uint(1); user <= 30; user++ {
		wg.Add(1)
		go func(id uint) {
			defer wg.Done()
			if err := service.Like(ctx, &Like{AccountID: id, VideoID: 1}); err != nil {
				t.Error(err)
			}
		}(user)
	}
	wg.Wait()
	assertCounts(t, s, 30, 0)
	// Repeated desired-state requests must not generate new counter events.
	for i := 0; i < 3; i++ {
		if err := service.Like(ctx, &Like{AccountID: 1, VideoID: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Unlike(ctx, &Like{AccountID: 1, VideoID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := service.Unlike(ctx, &Like{AccountID: 1, VideoID: 1}); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, s, 29, 0)
	var updates atomic.Int64
	s.db.Callback().Update().Before("gorm:update").Register("count:updates", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Video" {
			updates.Add(1)
		}
	})
	n, err := s.Flush(ctx, 1000)
	if err != nil || n != 31 {
		t.Fatalf("flush n=%d err=%v", n, err)
	}
	if updates.Load() != 1 {
		t.Fatalf("video updates=%d; want one coalesced update", updates.Load())
	}
	assertCounts(t, s, 29, 29)
	if n, err := s.Flush(ctx, 1000); err != nil || n != 0 {
		t.Fatalf("repeat flush=%d %v", n, err)
	}
	// Cache loss must reconstruct the persisted baseline and pending deltas.
	mr.FlushAll()
	if err := service.Like(ctx, &Like{AccountID: 31, VideoID: 1}); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, s, 30, 29)
	var events []LikeCountEvent
	if err := s.db.Order("id").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	// Delayed/repeated projection of already-flushed events cannot double count.
	if err := s.Apply(ctx, events); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, s, 30, 29)
	mr.FastForward(2 * time.Hour)
	assertCounts(t, s, 30, 29) // cold read includes the outstanding event
	if _, err := s.Flush(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, s, 30, 30)
}

func TestLikeCountRollbackAndConcurrentFlush(t *testing.T) {
	s, service, _ := countTestStore(t)
	ctx := context.Background()
	for user := uint(1); user <= 4; user++ {
		if err := service.Like(ctx, &Like{AccountID: user, VideoID: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a database failure after Redis has accepted all events.
	s.db.Callback().Update().Before("gorm:update").Register("count:fail", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Video" {
			tx.AddError(errors.New("injected failure"))
		}
	})
	if _, err := s.Flush(ctx, 100); err == nil {
		t.Fatal("expected flush failure")
	}
	assertCounts(t, s, 4, 0)
	s.db.Callback().Update().Remove("count:fail")
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Flush(ctx, 100); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	assertCounts(t, s, 4, 4)
}

func TestLikeCountRelationAndJournalRollback(t *testing.T) {
	s, service, _ := countTestStore(t)
	s.db.Callback().Create().Before("gorm:create").Register("count:fail-journal", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "LikeCountEvent" {
			tx.AddError(errors.New("journal unavailable"))
		}
	})
	if err := service.Like(context.Background(), &Like{AccountID: 1, VideoID: 1}); err == nil {
		t.Fatal("expected journal failure")
	}
	var n int64
	s.db.Model(&Like{}).Count(&n)
	if n != 0 {
		t.Fatal("relationship committed without its durable count event")
	}
	assertCounts(t, s, 0, 0)
}

func TestLikeCountRedisOutageKeepsJournal(t *testing.T) {
	s, service, mr := countTestStore(t)
	ctx := context.Background()
	mr.SetError("ERR simulated outage")
	if err := service.Like(ctx, &Like{AccountID: 1, VideoID: 1}); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, s, 1, 0) // durable pending delta is readable without Redis
	if _, err := s.Flush(ctx, 100); err == nil {
		t.Fatal("expected cache projection failure")
	}
	var pending int64
	s.db.Model(&LikeCountEvent{}).Where("flushed_at IS NULL").Count(&pending)
	if pending != 1 {
		t.Fatalf("pending=%d", pending)
	}
	mr.SetError("")
	if _, err := s.Flush(ctx, 100); err != nil {
		t.Fatal(err)
	}
	assertCounts(t, s, 1, 1)
}

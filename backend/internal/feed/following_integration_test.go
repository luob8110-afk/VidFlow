package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"feedsystem_video_go/internal/account"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"feedsystem_video_go/internal/social"
	"feedsystem_video_go/internal/video"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Set FEED_TEST_MYSQL_DSN to an isolated test database to run the real repository path.
func followingTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("FEED_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("FEED_TEST_MYSQL_DSN is unset")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("refresh_%d_", time.Now().UnixNano())}})
	if err != nil {
		t.Fatal(err)
	}
	models := []interface{}{&account.Account{}, &video.Video{}, &social.Social{}, &video.Like{}, &video.LikeCountEvent{}, &video.LikeCountFlushLock{}, &video.OutboxMsg{}, &video.Tag{}, &video.VideoTag{}}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(models...)
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	return db
}

func TestFollowingRefreshIntegration(t *testing.T) {
	db := followingTestDB(t)
	mr := miniredis.RunT(t)
	cache := rediscache.NewClient(goredis.NewClient(&goredis.Options{Addr: mr.Addr(), MaxRetries: -1}), "stage1:")
	t.Cleanup(func() { _ = cache.Close() })
	service := NewFeedService(NewFeedRepository(db), video.NewLikeRepository(db), cache)
	otherInstance := NewFeedService(NewFeedRepository(db), video.NewLikeRepository(db), cache)
	publisher := video.NewVideoService(video.NewVideoRepository(db), cache, nil)
	socialService := social.NewSocialService(social.NewSocialRepository(db), account.NewAccountRepository(db), nil, cache)
	ctx := context.Background()
	for _, user := range []account.Account{{ID: 1, Username: "reader"}, {ID: 2, Username: "author"}, {ID: 3, Username: "newreader"}} {
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, reader := range []uint{1, 3} {
		if err := socialService.Follow(ctx, &social.Social{FollowerID: reader, VloggerID: 2}); err != nil {
			t.Fatal(err)
		}
	}
	publish := func(title string) uint {
		t.Helper()
		item := video.Video{AuthorID: 2, Username: "author", Title: title, PlayURL: "/test.mp4", CoverURL: "/test.jpg", CreateTime: time.Now().Add(time.Duration(len(title)) * time.Second)}
		if err := publisher.Publish(ctx, &item); err != nil {
			t.Fatal(err)
		}
		return item.ID
	}
	assertFirst := func(response ListByFollowingResponse, err error, id uint) {
		t.Helper()
		if err != nil || len(response.VideoList) == 0 || response.VideoList[0].ID != id {
			t.Fatalf("expected first=%d response=%+v err=%v", id, response, err)
		}
	}
	oldID := publish("old")
	response, err := service.ListByFollowing(ctx, 10, time.Time{}, 1, false, nil)
	assertFirst(response, err, oldID)
	newID := publish("new video")
	response, err = service.ListByFollowing(ctx, 10, time.Time{}, 1, false, nil)
	assertFirst(response, err, oldID)
	response, err = service.ListByFollowing(ctx, 10, time.Time{}, 1, true, nil)
	assertFirst(response, err, newID)
	response, err = service.ListByFollowing(ctx, 10, time.Time{}, 3, true, nil)
	assertFirst(response, err, newID)
	t.Log("cached reader refresh and first visit see committed publication")

	// Freeze a database read after it captured old rows, then refresh on another instance.
	mr.FastForward(31 * time.Second)
	var armed atomic.Bool
	armed.Store(true)
	captured := make(chan struct{})
	release := make(chan struct{})
	if err := db.Callback().Query().After("gorm:query").Register("stage1:hold_old_read", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Video" && armed.CompareAndSwap(true, false) {
			close(captured)
			select {
			case <-release:
			case <-tx.Statement.Context.Done():
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove("stage1:hold_old_read")
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	done := make(chan error, 1)
	go func() { _, err := service.ListByFollowing(ctx, 10, time.Time{}, 1, false, nil); done <- err }()
	select {
	case <-captured:
	case <-time.After(time.Second):
		t.Fatal("old query was not captured")
	}
	latestID := publish("latest published video")
	response, err = otherInstance.ListByFollowing(ctx, 10, time.Time{}, 1, true, nil)
	assertFirst(response, err, latestID)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	response, err = service.ListByFollowing(ctx, 10, time.Time{}, 1, false, nil)
	assertFirst(response, err, latestID)
	t.Log("late query cannot restore stale cache across service instances")

	if err := socialService.Unfollow(ctx, &social.Social{FollowerID: 1, VloggerID: 2}); err != nil {
		t.Fatal(err)
	}
	response, err = service.ListByFollowing(ctx, 10, time.Time{}, 1, false, nil)
	if err != nil || len(response.VideoList) != 0 {
		t.Fatalf("unfollow returned old feed: %+v %v", response, err)
	}
	if err := socialService.Follow(ctx, &social.Social{FollowerID: 1, VloggerID: 2}); err != nil {
		t.Fatal(err)
	}
	response, err = service.ListByFollowing(ctx, 10, time.Time{}, 1, false, nil)
	assertFirst(response, err, latestID)
	t.Log("follow and unfollow invalidate cached membership")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("accountID", uint(1)) })
	router.POST("/feed/listByFollowing", NewFeedHandler(service).ListByFollowing)
	for _, body := range []string{`{"refresh":true,"latest_time":1}`, `{"latest_time":-1}`} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/feed/listByFollowing", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, req)
		if recorder.Code != 400 {
			t.Fatalf("invalid request accepted: %d %s", recorder.Code, recorder.Body)
		}
	}
	if err := db.Callback().Query().Before("gorm:query").Register("stage1:database_failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Video" {
			_ = tx.AddError(errors.New("injected database failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	failureRecorder := httptest.NewRecorder()
	failureRequest := httptest.NewRequest("POST", "/feed/listByFollowing", strings.NewReader(`{"refresh":true}`))
	failureRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(failureRecorder, failureRequest)
	if failureRecorder.Code != 500 {
		t.Fatalf("database failure returned successful stale data: %d %s", failureRecorder.Code, failureRecorder.Body)
	}
	db.Callback().Query().Remove("stage1:database_failure")
	t.Log("database failure is reported instead of a successful stale refresh")
	mr.Close()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/feed/listByFollowing", strings.NewReader(`{"refresh":true,"latest_time":0}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	if recorder.Code != 200 {
		t.Fatalf("refresh with Redis unavailable: %d %s", recorder.Code, recorder.Body)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	assertFirst(response, nil, latestID)
	t.Log("HTTP refresh validates parameters and works with Redis unavailable")
}

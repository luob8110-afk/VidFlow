package feed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rediscache "feedsystem_video_go/internal/middleware/redis"
	"feedsystem_video_go/internal/social"
	"feedsystem_video_go/internal/video"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

func TestFollowingPublicationRefreshPaginationFlow(t *testing.T) {
	db := followingTestDB(t)
	mr := miniredis.RunT(t)
	cache := rediscache.NewClient(goredis.NewClient(&goredis.Options{Addr: mr.Addr()}), "stage3:")
	t.Cleanup(func() { _ = cache.Close() })
	if err := db.Create(&social.Social{FollowerID: 1, VloggerID: 2}).Error; err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	expected := make([]uint, 0, 26)
	for i := 0; i < 25; i++ {
		item := video.Video{AuthorID: 2, Username: "author", Title: "old work", PlayURL: "/test.mp4", CoverURL: "/test.jpg", CreateTime: oldTime}
		if err := db.Create(&item).Error; err != nil {
			t.Fatal(err)
		}
		expected = append([]uint{item.ID}, expected...)
	}
	service := NewFeedService(NewFeedRepository(db), video.NewLikeRepository(db), cache)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("accountID", uint(1)) })
	router.POST("/feed", NewFeedHandler(service).ListByFollowing)
	request := func(cursor string, refresh bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]interface{}{"limit": 10, "cursor": cursor, "refresh": refresh})
		req := httptest.NewRequest("POST", "/feed", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	decode := func(recorder *httptest.ResponseRecorder) ListByFollowingResponse {
		t.Helper()
		if recorder.Code != 200 {
			t.Fatalf("HTTP %d %s", recorder.Code, recorder.Body)
		}
		var response ListByFollowingResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	initial := decode(request("", true))
	if len(initial.VideoList) != 10 || !initial.HasMore {
		t.Fatal("old page was not loaded")
	}
	var hold, fail atomic.Bool
	hold.Store(true)
	captured, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if err := db.Callback().Query().After("gorm:query").Register("stage3:hold", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Video" && hold.CompareAndSwap(true, false) {
			close(captured)
			select {
			case <-release:
			case <-tx.Statement.Context.Done():
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().Before("gorm:query").Register("stage3:fail", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Video" && fail.CompareAndSwap(true, false) {
			_ = tx.AddError(errors.New("injected refresh failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	late := make(chan *httptest.ResponseRecorder, 1)
	go func() { late <- request(initial.NextCursor, false) }()
	select {
	case <-captured:
	case <-time.After(time.Second):
		t.Fatal("pagination did not reach the database")
	}
	published := video.Video{AuthorID: 2, Username: "author", Title: "new work", PlayURL: "/test.mp4", CoverURL: "/test.jpg", CreateTime: time.Now()}
	publisher := video.NewVideoService(video.NewVideoRepository(db), cache, nil)
	if err := publisher.Publish(context.Background(), &published); err != nil {
		t.Fatal(err)
	}
	expected = append([]uint{published.ID}, expected...)
	fail.Store(true)
	if response := request("", true); response.Code != 500 {
		t.Fatalf("failed refresh returned %d", response.Code)
	}
	first := decode(request("", true))
	if first.VideoList[0].ID != published.ID {
		t.Fatal("retry did not show publication")
	}
	close(release)
	decode(<-late)
	if current := decode(request("", false)); current.VideoList[0].ID != published.ID {
		t.Fatal("late query restored old cache")
	}
	received := make([]uint, 0, 26)
	sizes := []int{}
	page := first
	for {
		sizes = append(sizes, len(page.VideoList))
		for _, item := range page.VideoList {
			received = append(received, item.ID)
		}
		if !page.HasMore {
			break
		}
		if len(sizes) > 3 {
			t.Fatal("pagination did not terminate")
		}
		page = decode(request(page.NextCursor, false))
	}
	if !reflect.DeepEqual(expected, received) || !reflect.DeepEqual(sizes, []int{10, 10, 6}) || page.NextCursor != "" {
		t.Fatalf("expected=%v received=%v sizes=%v", expected, received, sizes)
	}
	t.Logf("old_total=25 published=1 received=%d unique=26 page_sizes=%v order=pass late_query=pass failed_refresh_retry=pass", len(received), sizes)
}

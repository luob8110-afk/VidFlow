package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	rediscache "feedsystem_video_go/internal/middleware/redis"
	"feedsystem_video_go/internal/social"
	"feedsystem_video_go/internal/video"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"
)

func TestFeedOverlaysLiveCountOnCachedFollowingPage(t *testing.T) {
	db := followingTestDB(t)
	if err := db.AutoMigrate(&video.LikeCountEvent{}, &video.LikeCountFlushLock{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Migrator().DropTable(&video.LikeCountEvent{}, &video.LikeCountFlushLock{}) })
	mr := miniredis.RunT(t)
	cache := rediscache.NewClient(goredis.NewClient(&goredis.Options{Addr: mr.Addr()}), "feed-count:")
	t.Cleanup(func() { cache.Close() })
	v := video.Video{AuthorID: 2, Username: "author", Title: "test", PlayURL: "/test.mp4", CoverURL: "/cover.jpg", CreateTime: time.Now().Add(-time.Hour)}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&social.Social{FollowerID: 1, VloggerID: 2}).Error; err != nil {
		t.Fatal(err)
	}
	likes := video.NewLikeRepository(db)
	h := NewFeedHandler(NewFeedService(NewFeedRepository(db), likes, cache))
	request := func(following bool, want int64) {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/feed", bytes.NewBufferString(`{"limit":10}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("accountID", uint(1))
		if following {
			h.ListByFollowing(c)
		} else {
			h.ListLatest(c)
		}
		var response struct {
			VideoList []FeedVideoItem `json:"video_list"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || len(response.VideoList) != 1 || response.VideoList[0].LikesCount != want {
			t.Fatalf("feed response: %d %s", rec.Code, rec.Body.String())
		}
	}
	request(true, 0) // cache the page before the like
	service := video.NewLikeService(likes, video.NewVideoRepository(db), cache, nil, nil)
	if err := service.Like(context.Background(), &video.Like{AccountID: 3, VideoID: v.ID}); err != nil {
		t.Fatal(err)
	}
	request(true, 1)
	request(false, 1)
	var persisted video.Video
	db.First(&persisted, v.ID)
	if persisted.LikesCount != 0 {
		t.Fatal("request wrote through to database counter")
	}
}

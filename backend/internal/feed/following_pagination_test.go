package feed

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"sort"
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

func TestFollowingCursorPrecisionAndValidation(t *testing.T) {
	original := FollowingCursor{CreateTime: time.Date(2026, 11, 13, 12, 0, 0, 123456000, time.FixedZone("test", 8*3600)), ID: 9007199254740993}
	decoded, err := decodeFollowingCursor(original.Encode())
	if err != nil || !decoded.CreateTime.Equal(original.CreateTime) || decoded.ID != original.ID {
		t.Fatalf("cursor precision lost: %+v %v", decoded, err)
	}
	for _, raw := range []string{`{}`, `{"v":1,"time":"2026-11-13T00:00:00Z"}`, `{"v":1,"id":"1"}`, `{"v":2,"time":"2026-11-13T00:00:00Z","id":"1"}`, `{"v":1,"time":"bad","id":"1"}`, `{"v":1,"time":"2026-11-13T00:00:00Z","id":"0"}`} {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(raw))
		if _, err := decodeFollowingCursor(encoded); err == nil {
			t.Fatalf("accepted invalid cursor: %s", raw)
		}
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/feed", NewFeedHandler(nil).ListByFollowing)
	for _, body := range []map[string]interface{}{
		{"cursor": "broken!"}, {"cursor": strings.Repeat("a", 513)}, {"cursor": 123},
		{"cursor": original.Encode(), "refresh": true}, {"cursor": original.Encode(), "latest_time": 1},
	} {
		data, _ := json.Marshal(body)
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/feed", strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, req)
		if recorder.Code != 400 {
			t.Fatalf("expected 400: %s got %d", data, recorder.Code)
		}
	}
}

func TestFollowingPaginationIntegration(t *testing.T) {
	db := followingTestDB(t)
	// Existing databases may retain finer timestamp precision than GORM's default.
	statement := &gorm.Statement{DB: db}
	if err := statement.Parse(&video.Video{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE `" + statement.Schema.Table + "` MODIFY create_time DATETIME(6)").Error; err != nil {
		t.Fatal(err)
	}
	var videoQueries atomic.Int64
	if err := db.Callback().Query().After("gorm:query").Register("pagination:count", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Video" {
			videoQueries.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		name      string
		count     int
		timestamp func(int) time.Time
	}
	base := time.Date(2026, 11, 13, 12, 0, 0, 123456000, time.Local)
	tied := func(int) time.Time { return base }
	cases := []fixture{
		{"empty", 0, tied}, {"one", 1, tied}, {"exact_page", 10, tied}, {"page_plus_one", 11, tied},
		{"same_timestamp", 25, tied},
		{"same_second_milliseconds", 25, func(i int) time.Time { return base.Truncate(time.Second).Add(time.Duration(i) * time.Millisecond) }},
		{"mixed_seconds_and_ties", 100, func(i int) time.Time { return base.Add(time.Duration(i/4) * time.Second) }},
		{"same_millisecond_microseconds", 25, func(i int) time.Time { return base.Add(time.Duration(i) * time.Microsecond) }},
	}
	for index, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			reader := uint(100 + index)
			author := uint(1000 + index*10)
			for _, id := range []uint{author, author + 1} {
				if err := db.Create(&social.Social{FollowerID: reader, VloggerID: id}).Error; err != nil {
					t.Fatal(err)
				}
			}
			expectedVideos := make([]video.Video, 0, fixture.count)
			for i := 0; i < fixture.count; i++ {
				item := video.Video{AuthorID: author + uint(i%2), Username: "pagination", Title: fmt.Sprintf("work-%d", i), PlayURL: "/test.mp4", CoverURL: "/test.jpg", CreateTime: fixture.timestamp(i)}
				if err := db.Create(&item).Error; err != nil {
					t.Fatal(err)
				}
				expectedVideos = append(expectedVideos, item)
			}
			// An unfollowed author's newer and tied works must never enter the result.
			for _, when := range []time.Time{base.Add(time.Hour), base} {
				if err := db.Create(&video.Video{AuthorID: author + 2, Username: "hidden", Title: "unfollowed", PlayURL: "/test.mp4", CoverURL: "/test.jpg", CreateTime: when}).Error; err != nil {
					t.Fatal(err)
				}
			}
			sort.Slice(expectedVideos, func(i, j int) bool {
				if expectedVideos[i].CreateTime.Equal(expectedVideos[j].CreateTime) {
					return expectedVideos[i].ID > expectedVideos[j].ID
				}
				return expectedVideos[i].CreateTime.After(expectedVideos[j].CreateTime)
			})
			expected := make([]uint, 0, fixture.count)
			for _, item := range expectedVideos {
				expected = append(expected, item.ID)
			}
			mr := miniredis.RunT(t)
			cache := rediscache.NewClient(goredis.NewClient(&goredis.Options{Addr: mr.Addr(), MaxRetries: -1}), "pagination:")
			t.Cleanup(func() { _ = cache.Close() })
			service := NewFeedService(NewFeedRepository(db), video.NewLikeRepository(db), cache)
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Set("accountID", reader) })
			router.POST("/feed", NewFeedHandler(service).ListByFollowing)
			request := func(body interface{}) ListByFollowingResponse {
				t.Helper()
				data, _ := json.Marshal(body)
				req := httptest.NewRequest("POST", "/feed", strings.NewReader(string(data)))
				req.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, req)
				if recorder.Code != 200 {
					t.Fatalf("request failed: %d %s", recorder.Code, recorder.Body)
				}
				var response ListByFollowingResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				return response
			}
			for _, mode := range []string{"cold", "warm", "expired", "redis_unavailable"} {
				if mode == "expired" {
					mr.FastForward(31 * time.Second)
				}
				if mode == "redis_unavailable" {
					_ = cache.Close()
				}
				queriesBefore := videoQueries.Load()
				received := make([]uint, 0, fixture.count)
				sizes := []int{}
				cursor := ""
				for page := 0; page < 20; page++ {
					body := map[string]interface{}{"limit": 10, "cursor": cursor}
					response := request(body)
					replayed := request(body)
					if !reflect.DeepEqual(response, replayed) {
						t.Fatal("identical cursor returned a different page")
					}
					sizes = append(sizes, len(response.VideoList))
					for _, item := range response.VideoList {
						received = append(received, item.ID)
					}
					shouldHaveMore := len(received) < fixture.count
					if response.HasMore != shouldHaveMore || (response.NextCursor != "") != shouldHaveMore {
						t.Fatalf("incorrect end marker after %d: %+v", len(received), response)
					}
					if !response.HasMore {
						break
					}
					if response.NextCursor == cursor {
						t.Fatal("cursor did not advance")
					}
					cursor = response.NextCursor
				}
				if !reflect.DeepEqual(expected, received) {
					t.Fatalf("expected %v received %v", expected, received)
				}
				seen := map[uint]bool{}
				for _, id := range received {
					if seen[id] {
						t.Fatalf("duplicate id %d", id)
					}
					seen[id] = true
				}
				queryCount := videoQueries.Load() - queriesBefore
				if mode == "warm" && queryCount != 0 {
					t.Fatalf("warm pages queried database %d times", queryCount)
				}
				if mode != "warm" && queryCount == 0 {
					t.Fatalf("%s did not query database", mode)
				}
				t.Logf("mode=%s expected=%d received=%d unique=%d page_sizes=%v order=pass mysql_queries=%d", mode, fixture.count, len(received), len(seen), sizes, queryCount)
			}
			// Legacy callers keep the strict seconds-based boundary and receive next_time.
			legacy := request(map[string]interface{}{"limit": 10, "latest_time": base.Add(time.Second).Unix()})
			for _, item := range legacy.VideoList {
				if item.CreateTime >= base.Add(time.Second).Unix() {
					t.Fatal("legacy boundary changed")
				}
			}
		})
	}
}

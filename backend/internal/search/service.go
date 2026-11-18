package search

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"feedsystem_video_go/internal/feed"
	"feedsystem_video_go/internal/video"
)

var errCursorExpired = errors.New("search cursor expired")

type parameters struct {
	Query    string `json:"q"`
	Tag      string `json:"tag"`
	AuthorID uint   `json:"author_id"`
	From     int64  `json:"published_from"`
	Before   int64  `json:"published_before"`
	Limit    int    `json:"limit"`
}

func (p *parameters) normalize() bool {
	p.Query = strings.TrimSpace(p.Query)
	p.Tag = strings.TrimPrefix(strings.TrimSpace(p.Tag), "#")
	if p.Limit == 0 {
		p.Limit = 20
	}
	return (p.Query != "" || p.Tag != "" || p.AuthorID != 0 || p.From > 0 || p.Before > 0) &&
		utf8.RuneCountInString(p.Query) <= 100 && utf8.RuneCountInString(p.Tag) <= 100 &&
		p.From >= 0 && p.Before >= 0 && (p.Before == 0 || p.From < p.Before) && p.Limit >= 1 && p.Limit <= 50
}

type result struct {
	ID          uint      `json:"id"`
	AuthorID    uint      `json:"author_id"`
	Username    string    `json:"username"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	CoverURL    string    `json:"cover_url"`
	CreateTime  time.Time `json:"create_time"`
}

type page struct {
	Videos     []result `json:"video_list"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type service struct {
	es   *Elasticsearch
	repo *feed.FeedRepository
	key  []byte
}

type cursor struct {
	PIT   string            `json:"pit"`
	Sort  []json.RawMessage `json:"sort"`
	Query [sha256.Size]byte `json:"query"`
}

func querySignature(p parameters) [sha256.Size]byte {
	query, _ := json.Marshal(p)
	return sha256.Sum256(query)
}

func (s *service) seal(c cursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(s.mac(data)) + "." + base64.RawURLEncoding.EncodeToString(data)
}

func (s *service) unseal(token string, p parameters) (cursor, bool) {
	var c cursor
	mac, data, found := strings.Cut(token, ".")
	if !found || len(token) > 8192 {
		return c, false
	}
	provided, macErr := base64.RawURLEncoding.DecodeString(mac)
	decoded, dataErr := base64.RawURLEncoding.DecodeString(data)
	if macErr != nil || dataErr != nil || !hmac.Equal(provided, s.mac(decoded)) {
		return c, false
	}
	err := json.Unmarshal(decoded, &c)
	return c, err == nil && c.Query == querySignature(p) && c.PIT != "" && len(c.Sort) == 4
}

func (s *service) mac(data []byte) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("vidflow:video-search:v1\x00"))
	mac.Write(data)
	return mac.Sum(nil)
}

func Register(r *gin.Engine, db *gorm.DB, es *Elasticsearch, secret string) {
	key := []byte(secret)
	if len(key) == 0 {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
	}
	s := &service{es: es, repo: feed.NewFeedRepository(db), key: key}
	r.POST("/search/videos", s.handle)
}

func (s *service) handle(c *gin.Context) {
	if s.es == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "搜索服务暂未启用"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var req struct {
		parameters
		Cursor string `json:"cursor"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || !req.normalize() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入有效的关键词或筛选条件"})
		return
	}
	var position cursor
	if req.Cursor != "" {
		var ok bool
		if position, ok = s.unseal(req.Cursor, req.parameters); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "搜索游标无效，请重新搜索"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	result, err := s.search(ctx, req.parameters, position)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, result)
	case errors.Is(err, errCursorExpired):
		c.JSON(http.StatusGone, gin.H{"error": "搜索结果已过期，请重新搜索"})
	default:
		if c.Request.Context().Err() == nil {
			_ = c.Error(err)
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "搜索暂时不可用，请稍后重试"})
	}
}

func (p parameters) query() map[string]any {
	filters := []any{map[string]any{"exists": map[string]string{"field": "video_id"}}}
	if p.Tag != "" {
		filters = append(filters, map[string]any{"term": map[string]string{"tags": p.Tag}})
	}
	if p.AuthorID != 0 {
		filters = append(filters, map[string]any{"term": map[string]uint{"author_id": p.AuthorID}})
	}
	if p.From != 0 || p.Before != 0 {
		bounds := map[string]int64{}
		if p.From != 0 {
			bounds["gte"] = p.From
		}
		if p.Before != 0 {
			bounds["lt"] = p.Before
		}
		filters = append(filters, map[string]any{"range": map[string]any{"published_at": bounds}})
	}
	query := map[string]any{"filter": filters}
	if p.Query != "" {
		query["must"] = map[string]any{"multi_match": map[string]any{
			"query": p.Query, "fields": []string{"title^3", "description", "username"}, "operator": "and",
		}}
	}
	return map[string]any{"bool": query}
}

type hit struct {
	Data struct {
		ID uint `json:"video_id"`
	} `json:"_source"`
	Sort []json.RawMessage `json:"sort"`
}

func (s *service) search(ctx context.Context, p parameters, c cursor) (_ page, resultErr error) {
	output := page{Videos: make([]result, 0, p.Limit)}
	initial := c.PIT == ""
	if initial {
		var opened struct {
			ID string `json:"id"`
		}
		if err := s.es.request(ctx, http.MethodPost, "/"+indexAlias+"/_pit?keep_alive="+keepAlive+"&allow_partial_search_results=false", nil, &opened); err != nil {
			return page{}, err
		}
		c.PIT = opened.ID
		c.Query = querySignature(p)
	}
	defer func() {
		if (resultErr == nil && output.NextCursor == "") || (initial && resultErr != nil) {
			closeCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()
			_ = s.es.request(closeCtx, http.MethodDelete, "/_pit", map[string]string{"id": c.PIT}, nil)
		}
	}()
	// Normally this runs once. Scanning additional batches fills a page even if
	// older documents refer to rows that no longer exist in MySQL.
	for range 5 {
		hits, pitID, err := s.es.findHits(ctx, p, c)
		if esStatus(err, http.StatusNotFound) && !initial {
			return page{}, errCursorExpired
		}
		if err != nil {
			return page{}, err
		}
		if pitID != "" {
			c.PIT = pitID
		}
		byID, err := s.hydrate(ctx, hits)
		if err != nil {
			return page{}, err
		}
		for _, match := range hits {
			v := byID[match.Data.ID]
			if v != nil && len(output.Videos) == p.Limit {
				output.NextCursor = s.seal(c)
				return output, nil
			}
			c.Sort = match.Sort
			if v != nil {
				output.Videos = append(output.Videos, result{
					ID: v.ID, AuthorID: v.AuthorID, Username: v.Username, Title: v.Title,
					Description: v.Description, CoverURL: v.CoverURL, CreateTime: v.CreateTime,
				})
			}
		}
		if len(hits) <= p.Limit {
			return output, nil
		}
	}
	output.NextCursor = s.seal(c)
	return output, nil
}

func (es *Elasticsearch) findHits(ctx context.Context, p parameters, c cursor) ([]hit, string, error) {
	body := map[string]any{
		"pit":  map[string]string{"id": c.PIT, "keep_alive": keepAlive},
		"size": p.Limit + 1, "track_total_hits": false, "_source": []string{"video_id"},
		"query": p.query(), "sort": []any{
			map[string]string{"_score": "desc"}, map[string]string{"published_at": "desc"},
			map[string]string{"video_id": "desc"}, map[string]string{"_shard_doc": "asc"},
		},
	}
	if c.Sort != nil {
		body["search_after"] = c.Sort
	}
	var response struct {
		PIT  string `json:"pit_id"`
		Hits struct {
			Hits []hit `json:"hits"`
		} `json:"hits"`
	}
	err := es.request(ctx, http.MethodPost, "/_search?allow_partial_search_results=false", body, &response)
	return response.Hits.Hits, response.PIT, err
}

func (s *service) hydrate(ctx context.Context, hits []hit) (map[uint]*video.Video, error) {
	ids := make([]uint, 0, len(hits))
	for _, match := range hits {
		ids = append(ids, match.Data.ID)
	}
	videos, err := s.repo.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[uint]*video.Video, len(videos))
	for _, v := range videos {
		byID[v.ID] = v
	}
	return byID, nil
}

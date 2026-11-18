package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"feedsystem_video_go/internal/config"
)

const (
	indexAlias    = "vidflow-videos"
	physicalIndex = indexAlias + "-v1"
	keepAlive     = "2m"
)

type Elasticsearch struct {
	endpoint string
	apiKey   string
	http     *http.Client
}

func Open(cfg config.SearchConfig) (*Elasticsearch, error) {
	if cfg.Endpoint == "" {
		return nil, nil
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("SEARCH_ES_URL must be an HTTP(S) endpoint without credentials, query or fragment")
	}
	return &Elasticsearch{
		endpoint: strings.TrimRight(endpoint.String(), "/"), apiKey: cfg.APIKey,
		http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

type esError struct {
	status int
	detail string
}

func (e *esError) Error() string {
	return fmt.Sprintf("Elasticsearch HTTP %d: %s", e.status, e.detail)
}

func esStatus(err error, status int) bool {
	var response *esError
	return errors.As(err, &response) && response.status == status
}

func (es *Elasticsearch) request(ctx context.Context, method, path string, body any, result any) error {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	return es.send(ctx, method, path, "application/json", raw, result)
}

func (es *Elasticsearch) send(ctx context.Context, method, path, contentType string, body []byte, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, es.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", contentType)
	}
	if es.apiKey != "" {
		req.Header.Set("Authorization", "ApiKey "+es.apiKey)
	}
	res, err := es.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return &esError{status: res.StatusCode, detail: string(detail)}
	}
	if result == nil {
		_, err := io.Copy(io.Discard, res.Body)
		return err
	}
	return json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(result)
}

func (es *Elasticsearch) ensureIndex(ctx context.Context) error {
	err := es.request(ctx, http.MethodHead, "/_alias/"+indexAlias, nil, nil)
	if !esStatus(err, http.StatusNotFound) {
		return err
	}
	err = es.send(ctx, http.MethodPut, "/"+physicalIndex, "application/json", []byte(indexDefinition), nil)
	if err != nil {
		// Another worker may have installed the index between HEAD and PUT.
		if check := es.request(ctx, http.MethodHead, "/_alias/"+indexAlias, nil, nil); check == nil {
			return nil
		}
	}
	return err
}

const indexDefinition = `{
  "settings": {"number_of_shards": 1, "number_of_replicas": 0, "refresh_interval": "1s"},
  "aliases": {"vidflow-videos": {"is_write_index": true}},
  "mappings": {
    "dynamic": "strict",
    "properties": {
      "video_id": {"type": "long"},
      "author_id": {"type": "long"},
      "username": {"type": "text", "analyzer": "cjk"},
      "title": {"type": "text", "analyzer": "cjk"},
      "description": {"type": "text", "analyzer": "cjk"},
      "tags": {"type": "keyword", "normalizer": "lowercase"},
      "published_at": {"type": "date", "format": "epoch_millis"},
      "backfill_after": {"type": "long", "index": false},
      "backfill_through": {"type": "long", "index": false}
    }
  }
}`

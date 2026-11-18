package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"

	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	"feedsystem_video_go/internal/video"
)

type document struct {
	ID          uint     `json:"video_id"`
	AuthorID    uint     `json:"author_id"`
	Username    string   `json:"username"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	PublishedAt int64    `json:"published_at"`
}

type indexer struct {
	db *gorm.DB
	es *Elasticsearch
}

func RunIndexer(ctx context.Context, db *gorm.DB, es *Elasticsearch, cfg config.RabbitMQConfig) {
	index := &indexer{db: db, es: es}
	var startBackfill sync.Once
	rabbitmq.Reconnect(ctx, cfg, "video search indexer", func(mq *rabbitmq.RabbitMQ) error {
		ch, err := mq.NewChannel()
		if err != nil {
			return err
		}
		if err := rabbitmq.BindSearchQueue(ch); err != nil {
			return err
		}
		// Establish the durable subscription before choosing a historical cutoff.
		startBackfill.Do(func() { go index.backfill(ctx) })
		if err := ch.Qos(1, 0, false); err != nil {
			return err
		}
		deliveries, err := ch.Consume(rabbitmq.SearchQueue, "", false, false, false, false, nil)
		if err != nil {
			return err
		}
		log.Print("video search consuming ", rabbitmq.SearchQueue)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case delivery, ok := <-deliveries:
				if !ok {
					return errors.New("search consumer channel closed")
				}
				if err := index.consume(ctx, delivery); err != nil {
					// Closing the channel returns the unacknowledged delivery to RabbitMQ.
					return err
				}
			}
		}
	})
}

func (i *indexer) consume(ctx context.Context, delivery amqp.Delivery) error {
	var event rabbitmq.TimelineEvent
	if err := json.Unmarshal(delivery.Body, &event); err != nil || event.VideoID == 0 {
		return delivery.Reject(false)
	}
	opCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := i.indexVideo(opCtx, event.VideoID); err != nil {
		return fmt.Errorf("index video %d: %w", event.VideoID, err)
	}
	if err := delivery.Ack(false); err != nil {
		return err
	}
	if !event.OccurredAt.IsZero() {
		log.Printf("video indexed: video_id=%d event_id=%s lag_ms=%d redelivered=%t",
			event.VideoID, event.EventID, time.Since(event.OccurredAt).Milliseconds(), delivery.Redelivered)
	}
	return nil
}

func (i *indexer) indexVideo(ctx context.Context, id uint) error {
	if err := i.es.ensureIndex(ctx); err != nil {
		return err
	}
	path := "/" + indexAlias + "/_doc/" + strconv.FormatUint(uint64(id), 10)
	var current video.Video
	err := i.db.WithContext(ctx).First(&current, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err := i.es.request(ctx, http.MethodDelete, path, nil, nil)
		if esStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	docs, err := i.documents(ctx, []video.Video{current})
	if err != nil {
		return err
	}
	return i.es.request(ctx, http.MethodPut, path+"?require_alias=true", docs[0], nil)
}

func (i *indexer) documents(ctx context.Context, videos []video.Video) ([]document, error) {
	ids := make([]uint, 0, len(videos))
	for _, v := range videos {
		ids = append(ids, v.ID)
	}
	var rows []struct {
		VideoID uint
		Name    string
	}
	if err := i.db.WithContext(ctx).Table("video_tags").Select("video_tags.video_id, tags.name").
		Joins("JOIN tags ON tags.id = video_tags.tag_id").Where("video_tags.video_id IN ?", ids).
		Order("video_tags.id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	tags := make(map[uint][]string)
	for _, row := range rows {
		tags[row.VideoID] = append(tags[row.VideoID], row.Name)
	}
	docs := make([]document, 0, len(videos))
	for _, v := range videos {
		docs = append(docs, document{
			ID: v.ID, AuthorID: v.AuthorID, Username: v.Username, Title: v.Title,
			Description: v.Description, PublishedAt: v.CreateTime.UnixMilli(), Tags: tags[v.ID],
		})
	}
	return docs, nil
}

const checkpointID = "_backfill"

type backfillCheckpoint struct {
	After   uint `json:"backfill_after"`
	Through uint `json:"backfill_through"`
}

func (i *indexer) backfill(ctx context.Context) {
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		done, err := i.backfillBatch(opCtx)
		cancel()
		delay := 100 * time.Millisecond
		if err != nil && ctx.Err() == nil {
			log.Printf("video search backfill will retry: %v", err)
			delay = 3 * time.Second
		} else if done {
			delay = 30 * time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
	}
}

func (i *indexer) backfillBatch(ctx context.Context) (bool, error) {
	if err := i.es.ensureIndex(ctx); err != nil {
		return false, err
	}
	path := "/" + indexAlias + "/_doc/" + checkpointID
	var checkpoint struct {
		Sequence int64              `json:"_seq_no"`
		Term     int64              `json:"_primary_term"`
		State    backfillCheckpoint `json:"_source"`
	}
	err := i.es.request(ctx, http.MethodGet, path, nil, &checkpoint)
	if esStatus(err, http.StatusNotFound) {
		var upper uint
		if err := i.db.WithContext(ctx).Model(&video.Video{}).Select("COALESCE(MAX(id), 0)").Scan(&upper).Error; err != nil {
			return false, err
		}
		err := i.es.request(ctx, http.MethodPut, path+"?require_alias=true&op_type=create", backfillCheckpoint{Through: upper}, nil)
		if esStatus(err, http.StatusConflict) {
			return false, nil
		}
		return false, err
	}
	if err != nil || checkpoint.State.After >= checkpoint.State.Through {
		return err == nil, err
	}
	state := checkpoint.State
	var videos []video.Video
	if err := i.db.WithContext(ctx).Where("id > ? AND id <= ?", state.After, state.Through).
		Order("id").Limit(200).Find(&videos).Error; err != nil {
		return false, err
	}
	state.After = state.Through
	if len(videos) > 0 {
		docs, err := i.documents(ctx, videos)
		if err != nil {
			return false, err
		}
		if err := i.es.createBatch(ctx, docs); err != nil {
			return false, err
		}
		state.After = videos[len(videos)-1].ID
	}
	err = i.es.request(ctx, http.MethodPut, fmt.Sprintf("%s?require_alias=true&if_seq_no=%d&if_primary_term=%d",
		path, checkpoint.Sequence, checkpoint.Term), state, nil)
	if esStatus(err, http.StatusConflict) {
		return false, nil
	}
	if err == nil && state.After == state.Through {
		log.Printf("video search backfill completed: through_video_id=%d", state.Through)
	}
	return err == nil && state.After == state.Through, err
}

func (es *Elasticsearch) createBatch(ctx context.Context, docs []document) error {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, doc := range docs {
		if err := encoder.Encode(map[string]any{"create": map[string]string{"_id": strconv.FormatUint(uint64(doc.ID), 10)}}); err != nil {
			return err
		}
		if err := encoder.Encode(doc); err != nil {
			return err
		}
	}
	var result struct {
		Items []struct {
			Create struct {
				Status int             `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"create"`
		} `json:"items"`
	}
	if err := es.send(ctx, http.MethodPost, "/"+indexAlias+"/_bulk?require_alias=true&refresh=wait_for",
		"application/x-ndjson", body.Bytes(), &result); err != nil {
		return err
	}
	if len(result.Items) != len(docs) {
		return errors.New("incomplete Elasticsearch bulk response")
	}
	for _, item := range result.Items {
		if item.Create.Status != http.StatusCreated && item.Create.Status != http.StatusConflict {
			return fmt.Errorf("backfill bulk item returned %d: %s", item.Create.Status, item.Create.Error)
		}
	}
	return nil
}

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	oredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/middleware/rabbitmq"
	rediscache "feedsystem_video_go/internal/middleware/redis"
	"feedsystem_video_go/internal/video"
)

func RunPublicationRelay(ctx context.Context, db *gorm.DB, cfg config.RabbitMQConfig, searchEnabled bool) {
	rabbitmq.Reconnect(ctx, cfg, "video publication outbox", func(mq *rabbitmq.RabbitMQ) error {
		ch, err := mq.NewChannel()
		if err != nil {
			return err
		}
		publisher, err := rabbitmq.NewPublicationPublisher(ch, searchEnabled)
		if err != nil {
			return err
		}
		closed := ch.NotifyClose(make(chan *amqp.Error, 1))
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for ctx.Err() == nil {
			batchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := relayBatch(batchCtx, db, publisher)
			cancel()
			if err != nil {
				return err
			}
			select {
			case <-ctx.Done():
			case <-closed:
				return errors.New("publication channel closed")
			case <-tick.C:
			}
		}
		return ctx.Err()
	})
}

func relayBatch(ctx context.Context, db *gorm.DB, publisher *rabbitmq.PublicationPublisher) error {
	db = db.WithContext(ctx)
	var messages []video.OutboxMsg
	if err := db.Where("status = ? AND event_type = ?", "pending", video.EventPublished).
		Order("id").Limit(100).Find(&messages).Error; err != nil {
		return err
	}
	for _, msg := range messages {
		event := rabbitmq.TimelineEvent{
			EventID: fmt.Sprintf("video-published:%d", msg.ID), VideoID: msg.VideoID,
			CreateTime: msg.CreateTime.UnixMilli(), OccurredAt: msg.CreateTime,
		}
		if err := publisher.Publish(ctx, event); err != nil {
			return fmt.Errorf("outbox %d: %w", msg.ID, err)
		}
		if err := db.Delete(&msg).Error; err != nil {
			return err
		}
	}
	return nil
}

func RunTimelineConsumer(ctx context.Context, cfg config.RabbitMQConfig, cache *rediscache.Client) {
	rabbitmq.Reconnect(ctx, cfg, "Redis timeline consumer", func(mq *rabbitmq.RabbitMQ) error {
		ch, err := mq.NewChannel()
		if err != nil {
			return err
		}
		if err := ch.Qos(10, 0, false); err != nil {
			return err
		}
		deliveries, err := ch.Consume(rabbitmq.TimelineQueue, "", false, false, false, false, nil)
		if err != nil {
			return err
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case delivery, ok := <-deliveries:
				if !ok {
					return errors.New("timeline channel closed")
				}
				var event rabbitmq.TimelineEvent
				if err := json.Unmarshal(delivery.Body, &event); err != nil || event.VideoID == 0 {
					if err := delivery.Reject(false); err != nil {
						return err
					}
					continue
				}
				opCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
				key := cache.Key("feed:global_timeline")
				err := cache.ZAdd(opCtx, key, oredis.Z{
					Score: float64(event.CreateTime), Member: strconv.FormatUint(uint64(event.VideoID), 10),
				})
				if err == nil {
					err = cache.ZRemRangeByRank(opCtx, key, 0, -1001)
				}
				cancel()
				if err != nil {
					return err
				}
				if err := delivery.Ack(false); err != nil {
					return err
				}
			}
		}
	})
}

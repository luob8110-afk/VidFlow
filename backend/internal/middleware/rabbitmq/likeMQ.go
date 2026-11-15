package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type LikeMQ struct {
	ch *amqp.Channel
}

const (
	likeExchange   = "like.events"
	likeQueue      = "like.events"
	likeBindingKey = "like.*"

	likeLikeRK   = "like.like"
	likeUnlikeRK = "like.unlike"
)

type LikeEvent struct {
	NotificationOnly bool      `json:"notification_only,omitempty"`
	EventID          string    `json:"event_id"`
	Action           string    `json:"action"`
	UserID           uint      `json:"user_id"`
	VideoID          uint      `json:"video_id"`
	OccurredAt       time.Time `json:"occurred_at"`
}

func NewLikeMQ(base *RabbitMQ) (*LikeMQ, error) {
	if base == nil {
		return nil, errors.New("rabbitmq base is nil")
	}
	ch, err := base.NewChannel()
	if err != nil {
		return nil, err
	}
	if err := DeclareTopic(ch, likeExchange, likeQueue, likeBindingKey); err != nil {
		ch.Close()
		return nil, err
	}
	return &LikeMQ{ch: ch}, nil
}

func (l *LikeMQ) Like(ctx context.Context, userID, videoID uint) error {
	return l.publish(ctx, "like", likeLikeRK, userID, videoID, false)
}

func (l *LikeMQ) Unlike(ctx context.Context, userID, videoID uint) error {
	return l.publish(ctx, "unlike", likeUnlikeRK, userID, videoID, false)
}

func (l *LikeMQ) publish(ctx context.Context, action, routingKey string, userID, videoID uint, notificationOnly bool) error {
	if l == nil || l.ch == nil {
		return errors.New("like mq is not initialized")
	}
	if userID == 0 || videoID == 0 {
		return errors.New("userID and videoID are required")
	}
	id, err := newEventID(16)
	if err != nil {
		return err
	}
	event := LikeEvent{
		NotificationOnly: notificationOnly,
		EventID:          id,
		Action:           action,
		UserID:           userID,
		VideoID:          videoID,
		OccurredAt:       time.Now(),
	}
	return PublishJSON(ctx, l.ch, likeExchange, routingKey, event)
}

// Notify publishes an already committed relationship change. The relationship
// worker must not write it again; notification subscribers still receive it.
func (l *LikeMQ) Notify(ctx context.Context, userID, videoID uint, liked bool) error {
	if liked {
		return l.publish(ctx, "like", likeLikeRK, userID, videoID, true)
	}
	return l.publish(ctx, "unlike", likeUnlikeRK, userID, videoID, true)
}

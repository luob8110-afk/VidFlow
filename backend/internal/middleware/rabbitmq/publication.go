package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"feedsystem_video_go/internal/config"
)

const (
	publicationExchange = "video.timeline.events"
	publicationKey      = "video.timeline.publish"
	TimelineQueue       = "video.timeline.update.queue"
	SearchQueue         = "video.search.index.queue"
)

type TimelineEvent struct {
	EventID    string    `json:"event_id"`
	VideoID    uint      `json:"video_id"`
	CreateTime int64     `json:"create_time"`
	OccurredAt time.Time `json:"occurred_at"`
}

func BindSearchQueue(ch *amqp.Channel) error {
	deadQueue := SearchQueue + ".dlx"
	if _, err := ch.QueueDeclare(deadQueue, true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(SearchQueue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange": "", "x-dead-letter-routing-key": deadQueue,
	}); err != nil {
		return err
	}
	if err := ch.ExchangeDeclare(publicationExchange, "topic", true, false, false, false, nil); err != nil {
		return err
	}
	return ch.QueueBind(SearchQueue, publicationKey, publicationExchange, false, nil)
}

type PublicationPublisher struct {
	channel *amqp.Channel
	returns <-chan amqp.Return
}

func NewPublicationPublisher(ch *amqp.Channel, searchEnabled bool) (*PublicationPublisher, error) {
	if err := DeclareTopic(ch, publicationExchange, TimelineQueue, "video.timeline.*"); err != nil {
		return nil, err
	}
	if searchEnabled {
		if err := BindSearchQueue(ch); err != nil {
			return nil, err
		}
	}
	if err := ch.Confirm(false); err != nil {
		return nil, err
	}
	return &PublicationPublisher{channel: ch, returns: ch.NotifyReturn(make(chan amqp.Return, 1))}, nil
}

// Publish waits for one outstanding message per channel so a mandatory return can
// be correlated with the confirm. RabbitMQ sends the return before its confirm.
func (p *PublicationPublisher) Publish(ctx context.Context, event TimelineEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	confirmation, err := p.channel.PublishWithDeferredConfirmWithContext(ctx, publicationExchange, publicationKey, true, false, amqp.Publishing{
		MessageId: event.EventID, ContentType: "application/json", Body: body,
		DeliveryMode: amqp.Persistent, Timestamp: event.OccurredAt,
	})
	if err != nil {
		return err
	}
	ok, err := confirmation.WaitContext(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("publication was not confirmed by RabbitMQ")
	}
	select {
	case returned, open := <-p.returns:
		if !open {
			return errors.New("publication channel closed")
		}
		return fmt.Errorf("publication was unroutable: %s", returned.ReplyText)
	default:
		return nil
	}
}

func Reconnect(ctx context.Context, cfg config.RabbitMQConfig, name string, session func(*RabbitMQ) error) {
	for ctx.Err() == nil {
		mq, err := NewRabbitMQ(&cfg)
		if err == nil {
			err = session(mq)
			_ = mq.Close()
		}
		if ctx.Err() == nil {
			log.Printf("%s retrying in 3s: %v", name, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

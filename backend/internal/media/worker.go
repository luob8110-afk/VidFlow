package media

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/middleware/rabbitmq"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
)

const (
	queueName      = "media.process"
	maxAttempts    = 3
	processTimeout = 3 * time.Minute
	leaseDuration  = processTimeout + 20*time.Second
)

// Run opens its own reconnecting connection, so a broker restart can recover
// the media pipeline independently of the existing social consumers.
func (s *Service) Run(ctx context.Context, cfg config.RabbitMQConfig) {
	go s.runCleanup(ctx)
	for ctx.Err() == nil {
		err := s.session(ctx, cfg)
		if ctx.Err() == nil {
			log.Printf("media worker reconnecting: %v", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (s *Service) session(ctx context.Context, cfg config.RabbitMQConfig) error {
	mq, err := rabbitmq.NewRabbitMQ(&cfg)
	if err != nil {
		return err
	}
	defer mq.Close()
	publisher, err := mq.NewChannel()
	if err != nil {
		return err
	}
	if _, err := publisher.QueueDeclare(queueName, true, false, false, false, nil); err != nil {
		return err
	}
	if err := publisher.Confirm(false); err != nil {
		return err
	}
	returns := publisher.NotifyReturn(make(chan amqp.Return, 1))
	consumer, err := mq.NewChannel()
	if err != nil {
		return err
	}
	if err := consumer.Qos(1, 0, false); err != nil {
		return err
	}
	deliveries, err := consumer.Consume(queueName, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Print("media processor consuming ", queueName)
	sessionCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 2)
	go func() { done <- s.dispatchLoop(sessionCtx, publisher, returns) }()
	go func() { done <- s.consume(sessionCtx, deliveries) }()
	err = <-done
	cancel()
	<-done
	return err
}

func eligible(db *gorm.DB, now int64) *gorm.DB {
	return db.Where("((status = ? AND retry_at_millis <= ?) OR (status = ? AND lease_until_millis <= ?))",
		Queued, now, Processing, now)
}

func (s *Service) dispatchLoop(ctx context.Context, ch *amqp.Channel, returns <-chan amqp.Return) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.dispatch(opCtx, ch, returns)
		cancel()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return ctx.Err()
}

func (s *Service) dispatch(ctx context.Context, ch *amqp.Channel, returns <-chan amqp.Return) error {
	now := time.Now().UnixMilli()
	var assets []Asset
	if err := eligible(s.db.WithContext(ctx), now).Where("dispatch_at_millis <= ?", now).
		Order("dispatch_at_millis").Limit(16).Find(&assets).Error; err != nil {
		return err
	}
	for _, a := range assets {
		confirmation, err := ch.PublishWithDeferredConfirmWithContext(ctx, "", queueName, true, false, amqp.Publishing{
			MessageId: a.ID, ContentType: "text/plain", DeliveryMode: amqp.Persistent, Body: []byte(a.ID),
		})
		if err != nil {
			return err
		}
		ok, err := confirmation.WaitContext(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("media task was not confirmed by RabbitMQ")
		}
		// RabbitMQ sends a mandatory return before confirming an unrouted message.
		select {
		case returned, open := <-returns:
			if !open {
				return errors.New("media publisher channel closed")
			}
			return fmt.Errorf("media task unroutable: %s", returned.ReplyText)
		default:
		}
		// If no consumer claims the asset, resend after the acknowledgement window.
		// Matching the old timestamp prevents undoing a concurrent retry schedule.
		if err := s.db.WithContext(ctx).Model(&Asset{}).
			Where("id = ? AND status = ? AND dispatch_at_millis = ?", a.ID, a.Status, a.DispatchAtMillis).
			Update("dispatch_at_millis", time.Now().Add(5*time.Minute).UnixMilli()).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) consume(ctx context.Context, deliveries <-chan amqp.Delivery) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("media consumer channel closed")
			}
			id := string(delivery.Body)
			if !validHex(id, 32) {
				_ = delivery.Reject(false)
				continue
			}
			if err := s.handleTask(ctx, id); err != nil {
				// Closing the channel requeues the unacknowledged message.
				return err
			}
			if err := delivery.Ack(false); err != nil {
				return err
			}
		}
	}
}

func (s *Service) claim(ctx context.Context, id string) (Asset, bool, error) {
	now := time.Now().UnixMilli()
	db := s.db.WithContext(ctx).Model(&Asset{}).Where("id = ?", id)
	if err := eligible(db.Session(&gorm.Session{}), now).Where("attempts >= ?", maxAttempts).
		Updates(map[string]any{"status": Failed, "failure": "处理重试已用完，可重新上传",
			"lease_until_millis": nil, "lease_token": "", "dispatch_at_millis": nil, "retry_at_millis": nil,
			"expires_at_millis": now + draftLifetime.Milliseconds()}).Error; err != nil {
		return Asset{}, false, err
	}
	token, err := newID()
	if err != nil {
		return Asset{}, false, err
	}
	lease := now + leaseDuration.Milliseconds()
	res := eligible(db, now).Where("attempts < ?", maxAttempts).Updates(map[string]any{
		"status": Processing, "attempts": gorm.Expr("attempts + 1"), "lease_until_millis": lease,
		"lease_token": token, "dispatch_at_millis": lease, "failure": "",
	})
	if res.Error != nil || res.RowsAffected == 0 {
		return Asset{}, false, res.Error
	}
	var a Asset
	err = s.db.WithContext(ctx).Where("id = ? AND lease_token = ?", id, token).Take(&a).Error
	return a, err == nil, err
}

func (s *Service) handleTask(ctx context.Context, id string) error {
	a, claimed, err := s.claim(ctx, id)
	if err != nil || !claimed {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, processTimeout)
	output, processErr := s.process(opCtx, a)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if processErr != nil {
		log.Printf("media asset %s attempt %d: %v", a.ID, a.Attempts, processErr)
	}
	won, err := s.finishTask(ctx, a, output, processErr)
	if err != nil {
		return err
	}
	if won && processErr == nil {
		cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := s.storage.removePrefix(cleanupCtx, a.prefix()+"outputs/", a.output(a.LeaseToken, "")); err != nil {
			log.Printf("remove older media outputs for %s: %v", a.ID, err)
		}
	}
	return nil
}

func (s *Service) finishTask(ctx context.Context, a Asset, output result, processErr error) (bool, error) {
	now := time.Now().UnixMilli()
	values := map[string]any{"lease_until_millis": nil, "lease_token": "", "dispatch_at_millis": nil, "retry_at_millis": nil}
	switch {
	case processErr == nil:
		values["status"], values["output_token"] = Ready, a.LeaseToken
		values["duration_millis"], values["width"], values["height"] = output.DurationMillis, output.Width, output.Height
		values["expires_at_millis"] = now + draftLifetime.Milliseconds()
	case errors.Is(processErr, errChecksum) || errors.Is(processErr, errMedia):
		values["status"], values["failure"] = Failed, errMedia.Error()
		if errors.Is(processErr, errChecksum) {
			values["failure"] = errChecksum.Error()
		}
		values["expires_at_millis"] = now + draftLifetime.Milliseconds()
	case a.Attempts >= maxAttempts:
		values["status"], values["failure"] = Failed, "处理重试已用完，可重新上传"
		values["expires_at_millis"] = now + draftLifetime.Milliseconds()
	default:
		retry := now + (time.Duration(1<<a.Attempts) * time.Second).Milliseconds()
		values["status"], values["retry_at_millis"], values["dispatch_at_millis"] = Queued, retry, retry
	}
	res := s.db.WithContext(ctx).Model(&Asset{}).
		Where("id = ? AND status = ? AND lease_token = ? AND lease_until_millis > ?", a.ID, Processing, a.LeaseToken, now).
		Updates(values)
	return res.RowsAffected != 0, res.Error
}

func (s *Service) runCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := s.cleanup(opCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("media cleanup: %v", err)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (s *Service) cleanup(ctx context.Context) error {
	db := s.db.WithContext(ctx)
	now := time.Now().UnixMilli()
	if err := db.Model(&Asset{}).Where("status IN ? AND expires_at_millis <= ?", []Status{Uploading, Failed, Ready}, now).
		Update("status", Expired).Error; err != nil {
		return err
	}
	var expired []Asset
	if err := db.Where("status = ? AND cleaned_at IS NULL", Expired).Order("updated_at").Limit(16).Find(&expired).Error; err != nil {
		return err
	}
	for _, a := range expired {
		if err := s.storage.discard(ctx, a); err != nil {
			log.Printf("media cleanup %s: %v", a.ID, err)
			continue
		}
		if err := db.Model(&a).Update("cleaned_at", time.Now()).Error; err != nil {
			return err
		}
	}
	return ctx.Err()
}

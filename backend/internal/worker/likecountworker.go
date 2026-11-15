package worker

import (
	"context"
	"log"
	"time"

	"feedsystem_video_go/internal/video"
)

func RunLikeCountFlusher(ctx context.Context, store *video.LikeCountStore, interval time.Duration, batchSize int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for ctx.Err() == nil {
			opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			n, err := store.Flush(opCtx, batchSize)
			cancel()
			if err != nil {
				log.Printf("like count flush failed (journal retained): %v", err)
				break
			}
			if n < batchSize {
				break
			}
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := store.Cleanup(cleanupCtx); err != nil {
			log.Printf("like count journal cleanup: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

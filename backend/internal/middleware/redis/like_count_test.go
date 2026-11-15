package redis

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func TestLikeCounterAtomicDedupAndExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	c := NewClient(goredis.NewClient(&goredis.Options{Addr: mr.Addr()}), "test:")
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	// The bootstrap snapshot already includes event 1.
	if err := c.ApplyLikeCount(ctx, 1, 11, []uint64{1}, 1, 1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.ApplyLikeCount(ctx, 1, 0, nil, 2, -1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	counts, err := c.LikeCounts(ctx, []uint{1})
	if err != nil || counts[1] != 10 {
		t.Fatalf("counts=%v err=%v", counts, err)
	}
	mr.FastForward(2 * time.Hour)
	if err := c.ApplyLikeCount(ctx, 1, 0, nil, 3, 1); err == nil {
		t.Fatal("expired counter must require a baseline, never reset to zero")
	}
	if err := c.ApplyLikeCount(ctx, 1, 12, []uint64{1, 2, 3}, 3, 1); err != nil {
		t.Fatal(err)
	}
	counts, err = c.LikeCounts(ctx, []uint{1})
	if err != nil || counts[1] != 12 {
		t.Fatalf("rehydrated=%v err=%v", counts, err)
	}
}

package redis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func TestVersionedCacheRejectsLateReadsAndWrites(t *testing.T) {
	mr := miniredis.RunT(t)
	client := NewClient(goredis.NewClient(&goredis.Options{Addr: mr.Addr()}), "test:")
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	versionKey := client.FollowingFeedVersionKey(1)
	old, err := client.CacheVersion(ctx, versionKey, false, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetVersionedBytes(ctx, versionKey, "page", old, []byte("old"), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	current, err := client.CacheVersion(ctx, versionKey, true, time.Hour)
	if err != nil || current == old {
		t.Fatalf("rotation: %q %v", current, err)
	}
	if _, err := client.GetVersionedBytes(ctx, versionKey, "page", old); !IsMiss(err) {
		t.Fatalf("old version read: %v", err)
	}
	if err := client.SetVersionedBytes(ctx, versionKey, "page", current, []byte("new"), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := client.SetVersionedBytes(ctx, versionKey, "page", old, []byte("late old"), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	value, err := client.GetVersionedBytes(ctx, versionKey, "page", current)
	if err != nil || string(value) != "new" {
		t.Fatalf("late write replaced current page: %q %v", value, err)
	}
	mr.FastForward(31 * time.Second)
	if _, err := client.GetVersionedBytes(ctx, versionKey, "page", current); !IsMiss(err) {
		t.Fatalf("expired page read: %v", err)
	}
	mr.FastForward(time.Hour)
	recreated, err := client.CacheVersion(ctx, versionKey, false, time.Hour)
	if err != nil || recreated == current || recreated == old {
		t.Fatalf("expired version reused: %q %v", recreated, err)
	}
}

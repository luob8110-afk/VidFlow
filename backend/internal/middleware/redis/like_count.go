package redis

import (
	"context"
	"errors"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// The total and deduplication markers expire together. A fixed lifetime bounds
// memory; the durable journal is retained longer than this cache generation.
const LikeCountTTL = time.Hour

func (c *Client) LikeCountKey(id uint) string { return c.Key("like:count:v1:%d", id) }

var applyLikeCount = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  if ARGV[5] == '0' then return redis.error_reply('counter expired; retry hydration') end
  redis.call('HSET', KEYS[1], 'total', ARGV[1])
  for i = 6, #ARGV do redis.call('HSET', KEYS[1], 'e:' .. ARGV[i], 1) end
  redis.call('PEXPIRE', KEYS[1], ARGV[4])
end
if ARGV[2] ~= '' and redis.call('HSETNX', KEYS[1], 'e:' .. ARGV[2], 1) == 1 then
  redis.call('HINCRBY', KEYS[1], 'total', ARGV[3])
end
return redis.call('HGET', KEYS[1], 'total')
`)

func (c *Client) ApplyLikeCount(ctx context.Context, videoID uint, baseline int64, included []uint64, eventID uint64, delta int64) error {
	if c == nil || c.rdb == nil {
		return errors.New("redis client not initialized")
	}
	id := ""
	if eventID != 0 {
		id = strconv.FormatUint(eventID, 10)
	}
	hydrated := 0
	if included != nil {
		hydrated = 1
	}
	args := []interface{}{baseline, id, delta, LikeCountTTL.Milliseconds(), hydrated}
	for _, item := range included {
		args = append(args, strconv.FormatUint(item, 10))
	}
	return applyLikeCount.Run(ctx, c.rdb, []string{c.LikeCountKey(videoID)}, args...).Err()
}

func (c *Client) LikeCounts(ctx context.Context, ids []uint) (map[uint]int64, error) {
	result := make(map[uint]int64)
	if c == nil || c.rdb == nil {
		return result, nil
	}
	p := c.rdb.Pipeline()
	cmds := make([]*goredis.StringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = p.HGet(ctx, c.LikeCountKey(id), "total")
	}
	_, err := p.Exec(ctx)
	if err != nil && err != goredis.Nil {
		return result, err
	}
	for i, cmd := range cmds {
		if n, err := cmd.Int64(); err == nil {
			if n < 0 {
				n = 0
			}
			result[ids[i]] = n
		}
	}
	return result, nil
}

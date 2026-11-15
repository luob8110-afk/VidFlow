package redis

import (
	"context"
	"errors"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

var cacheVersionScript = goredis.NewScript(`
local version = redis.call('GET', KEYS[1])
if ARGV[1] == 'true' or not version then
  version = ARGV[2]
  redis.call('SET', KEYS[1], version, 'PX', ARGV[3])
end
return version
`)

var getVersionedCacheScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return false
end
return redis.call('GET', KEYS[2])
`)

var setVersionedCacheScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call('SET', KEYS[2], ARGV[2], 'PX', ARGV[3])
return 1
`)

// CacheVersion creates or rotates a random version so expired versions cannot be reused.
func (c *Client) CacheVersion(ctx context.Context, key string, rotate bool, ttl time.Duration) (string, error) {
	if c == nil || c.rdb == nil {
		return "", errors.New("redis client not initialized")
	}
	token, err := randToken(16)
	if err != nil {
		return "", err
	}
	return cacheVersionScript.Run(ctx, c.rdb, []string{key}, strconv.FormatBool(rotate), token, ttl.Milliseconds()).Text()
}

func (c *Client) GetVersionedBytes(ctx context.Context, versionKey, key, version string) ([]byte, error) {
	if c == nil || c.rdb == nil {
		return nil, errors.New("redis client not initialized")
	}
	value, err := getVersionedCacheScript.Run(ctx, c.rdb, []string{versionKey, key}, version).Text()
	return []byte(value), err
}

// SetVersionedBytes checks the version and writes in one atomic Redis operation.
func (c *Client) SetVersionedBytes(ctx context.Context, versionKey, key, version string, value []byte, ttl time.Duration) error {
	if c == nil || c.rdb == nil {
		return errors.New("redis client not initialized")
	}
	if ttl < time.Millisecond {
		return nil
	}
	return setVersionedCacheScript.Run(ctx, c.rdb, []string{versionKey, key}, version, value, ttl.Milliseconds()).Err()
}

func (c *Client) FollowingFeedVersionKey(accountID uint) string {
	return c.Key("feed:following:v2:account=%d:version", accountID)
}

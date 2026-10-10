package repository

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

var _ service.SessionActivityCache = (*gatewayCache)(nil)

func sessionActivityKeys(groupID int64, sessionKey string) []string {
	// Both keys occupy the same Redis cluster slot. Hashing also bounds key size.
	scope := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", groupID, sessionKey)))
	prefix := fmt.Sprintf("session_activity:{%x}:", scope)
	return []string{prefix + "state", prefix + "active"}
}

// A disappeared worker's expired lease is uncertainty, not proof of inactivity.
// Start a fresh idle window when pruning it. Use Redis TIME across instances.
const sessionActivityPrune = `
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
if redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now) > 0 then
  redis.call('HSET', KEYS[1], 'last', now)
  redis.call('HDEL', KEYS[1], 'owner')
  redis.call('PEXPIRE', KEYS[1], ARGV[#ARGV])
end
`

var beginSessionActivityScript = redis.NewScript(sessionActivityPrune + `
local last = tonumber(redis.call('HGET', KEYS[1], 'last'))
local idle = last and now - last >= tonumber(ARGV[2]) and redis.call('ZCARD', KEYS[2]) == 0
redis.call('HDEL', KEYS[1], 'owner')
if idle then redis.call('HSET', KEYS[1], 'owner', ARGV[1]) end
redis.call('HSET', KEYS[1], 'last', now)
redis.call('ZADD', KEYS[2], now + tonumber(ARGV[3]), ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
if idle then return 1 else return 0 end
`)

var refreshSessionActivityScript = redis.NewScript(sessionActivityPrune + `
redis.call('HSET', KEYS[1], 'last', now)
redis.call('ZADD', KEYS[2], now + tonumber(ARGV[2]), ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
redis.call('PEXPIRE', KEYS[2], ARGV[3])
return 1
`)

var endSessionActivityScript = redis.NewScript(`
if redis.call('ZREM', KEYS[2], ARGV[1]) == 0 then return 0 end
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
redis.call('HSET', KEYS[1], 'last', now)
redis.call('HDEL', KEYS[1], 'owner')
redis.call('PEXPIRE', KEYS[1], ARGV[2])
redis.call('PEXPIRE', KEYS[2], ARGV[2])
return 1
`)

var sessionActivityAllowsMigrationScript = redis.NewScript(sessionActivityPrune + `
if redis.call('HGET', KEYS[1], 'owner') == ARGV[1] and
   redis.call('ZCARD', KEYS[2]) == 1 and redis.call('ZSCORE', KEYS[2], ARGV[1]) then
  return 1
end
return 0
`)

func (c *gatewayCache) BeginSessionActivity(ctx context.Context, groupID int64, sessionKey, requestID string) (bool, error) {
	n, err := beginSessionActivityScript.Run(ctx, c.rdb, sessionActivityKeys(groupID, sessionKey), requestID,
		service.SessionActivityIdleWindow.Milliseconds(), service.SessionActivityLease.Milliseconds(), service.SessionActivityRetention.Milliseconds()).Int()
	return n == 1, err
}

func (c *gatewayCache) RefreshSessionActivity(ctx context.Context, groupID int64, sessionKey, requestID string) error {
	return refreshSessionActivityScript.Run(ctx, c.rdb, sessionActivityKeys(groupID, sessionKey), requestID,
		service.SessionActivityLease.Milliseconds(), service.SessionActivityRetention.Milliseconds()).Err()
}

func (c *gatewayCache) EndSessionActivity(ctx context.Context, groupID int64, sessionKey, requestID string) error {
	return endSessionActivityScript.Run(ctx, c.rdb, sessionActivityKeys(groupID, sessionKey), requestID,
		service.SessionActivityRetention.Milliseconds()).Err()
}

func (c *gatewayCache) SessionActivityAllowsMigration(ctx context.Context, groupID int64, sessionKey, requestID string) (bool, error) {
	n, err := sessionActivityAllowsMigrationScript.Run(ctx, c.rdb, sessionActivityKeys(groupID, sessionKey), requestID,
		service.SessionActivityRetention.Milliseconds()).Int()
	return n == 1, err
}

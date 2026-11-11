//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type rateLimitUsageCacheStub struct {
	billingCacheWorkerStub
	updateErr   error
	updates     int64
	lastCost    atomic.Value
	invalidates int64
}

func (c *rateLimitUsageCacheStub) UpdateAPIKeyRateLimitUsage(ctx context.Context, keyID int64, cost float64) error {
	atomic.AddInt64(&c.updates, 1)
	c.lastCost.Store(cost)
	return c.updateErr
}

func (c *rateLimitUsageCacheStub) InvalidateAPIKeyRateLimit(ctx context.Context, keyID int64) error {
	atomic.AddInt64(&c.invalidates, 1)
	return nil
}

// fullQueueBillingCacheService 返回一个写队列永远满（无缓冲且没有消费者）的服务。
func fullQueueBillingCacheService(cache BillingCache) *BillingCacheService {
	return &BillingCacheService{cache: cache, cacheWriteChan: make(chan cacheWriteTask)}
}

func TestQueueUpdateAPIKeyRateLimitUsage_QueueFullFallsBackToSyncWrite(t *testing.T) {
	cache := &rateLimitUsageCacheStub{}
	svc := fullQueueBillingCacheService(cache)

	svc.QueueUpdateAPIKeyRateLimitUsage(11, 1.25)

	require.Equal(t, int64(1), atomic.LoadInt64(&cache.updates), "queue-full increment must be written synchronously")
	require.Equal(t, 1.25, cache.lastCost.Load())
	require.Equal(t, int64(0), atomic.LoadInt64(&cache.invalidates), "successful write must keep the cache entry")
}

func TestQueueUpdateAPIKeyRateLimitUsage_SyncWriteFailureInvalidatesCache(t *testing.T) {
	cache := &rateLimitUsageCacheStub{updateErr: errors.New("redis down")}
	svc := fullQueueBillingCacheService(cache)

	svc.QueueUpdateAPIKeyRateLimitUsage(11, 2)

	require.Equal(t, int64(1), atomic.LoadInt64(&cache.updates))
	require.Equal(t, int64(1), atomic.LoadInt64(&cache.invalidates), "failed write must drop the stale cache so the next check reloads from DB")
}

func TestCacheWriteWorker_RateLimitUsageFailureInvalidatesCache(t *testing.T) {
	cache := &rateLimitUsageCacheStub{updateErr: errors.New("redis timeout")}
	svc := &BillingCacheService{cache: cache}

	ch := make(chan cacheWriteTask, 1)
	ch <- cacheWriteTask{kind: cacheWriteUpdateRateLimitUsage, apiKeyID: 5, amount: 3}
	close(ch)
	svc.cacheWriteWg.Add(1)
	svc.cacheWriteWorker(ch)

	require.Equal(t, int64(1), atomic.LoadInt64(&cache.updates))
	require.Equal(t, int64(1), atomic.LoadInt64(&cache.invalidates))
}

func TestCacheWriteWorker_RateLimitUsageSuccessKeepsCache(t *testing.T) {
	cache := &rateLimitUsageCacheStub{}
	svc := &BillingCacheService{cache: cache}

	ch := make(chan cacheWriteTask, 1)
	ch <- cacheWriteTask{kind: cacheWriteUpdateRateLimitUsage, apiKeyID: 5, amount: 3}
	close(ch)
	svc.cacheWriteWg.Add(1)
	svc.cacheWriteWorker(ch)

	require.Equal(t, int64(1), atomic.LoadInt64(&cache.updates))
	require.Equal(t, int64(0), atomic.LoadInt64(&cache.invalidates))
}

//go:build unit

package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type rateLimitResetCacheStub struct {
	billingCacheWorkerStub
	mu          sync.Mutex
	entry       *APIKeyRateLimitCacheData
	gets        int64
	invalidates int64
}

func (c *rateLimitResetCacheStub) GetAPIKeyRateLimit(ctx context.Context, keyID int64) (*APIKeyRateLimitCacheData, error) {
	atomic.AddInt64(&c.gets, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entry == nil {
		return nil, context.Canceled
	}
	cp := *c.entry
	return &cp, nil
}

func (c *rateLimitResetCacheStub) SetAPIKeyRateLimit(ctx context.Context, keyID int64, data *APIKeyRateLimitCacheData) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *data
	c.entry = &cp
	return nil
}

func (c *rateLimitResetCacheStub) InvalidateAPIKeyRateLimit(ctx context.Context, keyID int64) error {
	atomic.AddInt64(&c.invalidates, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry = nil
	return nil
}

type rateLimitResetLoaderStub struct {
	data    *APIKeyRateLimitData
	loads   int64
	resets  int64
	release chan struct{}
}

func (l *rateLimitResetLoaderStub) GetRateLimitData(context.Context, int64) (*APIKeyRateLimitData, error) {
	atomic.AddInt64(&l.loads, 1)
	cp := *l.data
	return &cp, nil
}

func (l *rateLimitResetLoaderStub) ResetRateLimitWindows(ctx context.Context, id int64) error {
	atomic.AddInt64(&l.resets, 1)
	if l.release != nil {
		select {
		case <-l.release:
		case <-ctx.Done():
		}
	}
	return nil
}

func newRateLimitResetTestService(t *testing.T, cache *rateLimitResetCacheStub, loader *rateLimitResetLoaderStub) *BillingCacheService {
	t.Helper()
	svc := NewBillingCacheService(cache, nil, nil, nil, nil, nil, &config.Config{}, nil)
	svc.apiKeyRateLimitLoader = loader
	t.Cleanup(svc.Stop)
	return svc
}

func TestCheckAPIKeyRateLimits_NilWindowsDoNotTriggerReset(t *testing.T) {
	cache := &rateLimitResetCacheStub{}
	loader := &rateLimitResetLoaderStub{data: &APIKeyRateLimitData{}}
	svc := newRateLimitResetTestService(t, cache, loader)
	apiKey := &APIKey{ID: 42, RateLimit5h: 10, RateLimit1d: 20, RateLimit7d: 30}

	for i := 0; i < 100; i++ {
		require.NoError(t, svc.checkAPIKeyRateLimits(context.Background(), apiKey))
	}
	// Give any (incorrectly) spawned reset goroutine a chance to run.
	time.Sleep(50 * time.Millisecond)

	require.Equal(t, int64(0), atomic.LoadInt64(&loader.resets), "NULL windows must not trigger a DB reset")
	require.Equal(t, int64(0), atomic.LoadInt64(&cache.invalidates), "NULL windows must not drop the cache entry")
	require.Equal(t, int64(1), atomic.LoadInt64(&loader.loads), "only the first cache miss should hit the DB")
}

func TestCheckAPIKeyRateLimits_NilWindowStillCountsAsZeroUsage(t *testing.T) {
	cache := &rateLimitResetCacheStub{entry: &APIKeyRateLimitCacheData{Usage5h: 100}}
	loader := &rateLimitResetLoaderStub{data: &APIKeyRateLimitData{}}
	svc := newRateLimitResetTestService(t, cache, loader)
	apiKey := &APIKey{ID: 7, RateLimit5h: 10}

	require.NoError(t, svc.checkAPIKeyRateLimits(context.Background(), apiKey))
	require.Equal(t, int64(0), atomic.LoadInt64(&loader.resets))
}

func TestCheckAPIKeyRateLimits_ExpiredWindowResetIsDeduplicated(t *testing.T) {
	expired := time.Now().Add(-6 * time.Hour).Unix()
	fresh := time.Now().Unix()
	cache := &rateLimitResetCacheStub{entry: &APIKeyRateLimitCacheData{
		Usage5h: 100, Usage1d: 1, Usage7d: 1,
		Window5h: expired, Window1d: fresh, Window7d: fresh,
	}}
	loader := &rateLimitResetLoaderStub{data: &APIKeyRateLimitData{}, release: make(chan struct{})}
	svc := newRateLimitResetTestService(t, cache, loader)
	apiKey := &APIKey{ID: 9, RateLimit5h: 10}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Expired 5h usage is treated as 0, so the request is allowed.
			require.NoError(t, svc.checkAPIKeyRateLimits(context.Background(), apiKey))
		}()
	}
	wg.Wait()

	require.Eventually(t, func() bool { return atomic.LoadInt64(&loader.resets) == 1 }, time.Second, 5*time.Millisecond)
	close(loader.release)
	require.Eventually(t, func() bool { return atomic.LoadInt64(&cache.invalidates) == 1 }, time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		_, inFlight := svc.rateLimitResetInFlight.Load(apiKey.ID)
		return !inFlight
	}, time.Second, 5*time.Millisecond)
	require.Equal(t, int64(1), atomic.LoadInt64(&loader.resets), "concurrent checks must share one reset")

	// After the in-flight reset finished, a later expiry may trigger a new reset.
	cache.mu.Lock()
	cache.entry = &APIKeyRateLimitCacheData{Usage5h: 100, Window5h: expired, Window1d: fresh, Window7d: fresh}
	cache.mu.Unlock()
	require.NoError(t, svc.checkAPIKeyRateLimits(context.Background(), apiKey))
	require.Eventually(t, func() bool { return atomic.LoadInt64(&loader.resets) == 2 }, time.Second, 5*time.Millisecond)
}

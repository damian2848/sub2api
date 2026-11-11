package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newRateLimitWindowCache(t *testing.T) (service.BillingCache, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewBillingCache(client), server
}

// The DB opens the 1d and 7d windows at the start of the day, not at the
// current second. The cache must not open a window of its own: an entry with
// an unopened window is dropped on the first usage update, so the next check
// reloads the windows the billing transaction wrote.
func TestUpdateAPIKeyRateLimitUsage_UnopenedWindowDropsEntry(t *testing.T) {
	ctx := context.Background()
	cache, server := newRateLimitWindowCache(t)

	// What a DB reload caches for a key that has never been used.
	require.NoError(t, cache.SetAPIKeyRateLimit(ctx, 7, &service.APIKeyRateLimitCacheData{}))
	require.NoError(t, cache.UpdateAPIKeyRateLimitUsage(ctx, 7, 9))

	require.False(t, server.Exists(billingRateLimitKey(7)), "the cache must not start a window at the current second")
	data, err := cache.GetAPIKeyRateLimit(ctx, 7)
	if err == nil {
		require.Nil(t, data)
	}
}

func TestUpdateAPIKeyRateLimitUsage_ExpiredWindowDropsEntry(t *testing.T) {
	ctx := context.Background()
	cache, server := newRateLimitWindowCache(t)
	now := time.Now()

	require.NoError(t, cache.SetAPIKeyRateLimit(ctx, 8, &service.APIKeyRateLimitCacheData{
		Usage5h: 3, Usage1d: 3, Usage7d: 3,
		Window5h: now.Add(-6 * time.Hour).Unix(), // expired
		Window1d: now.Add(-6 * time.Hour).Unix(),
		Window7d: now.Add(-6 * time.Hour).Unix(),
	}))
	require.NoError(t, cache.UpdateAPIKeyRateLimitUsage(ctx, 8, 1))
	require.False(t, server.Exists(billingRateLimitKey(8)), "a rolled window is reloaded from the DB, not restarted in the cache")
}

func TestUpdateAPIKeyRateLimitUsage_LiveWindowsAccumulate(t *testing.T) {
	ctx := context.Background()
	cache, _ := newRateLimitWindowCache(t)
	now := time.Now()
	dayStart := now.Add(-3 * time.Hour).Unix()

	require.NoError(t, cache.SetAPIKeyRateLimit(ctx, 9, &service.APIKeyRateLimitCacheData{
		Usage5h: 1, Usage1d: 2, Usage7d: 3,
		Window5h: now.Add(-time.Hour).Unix(), Window1d: dayStart, Window7d: dayStart,
	}))
	require.NoError(t, cache.UpdateAPIKeyRateLimitUsage(ctx, 9, 0.5))

	data, err := cache.GetAPIKeyRateLimit(ctx, 9)
	require.NoError(t, err)
	require.NotNil(t, data)
	require.InDelta(t, 1.5, data.Usage5h, 1e-9)
	require.InDelta(t, 2.5, data.Usage1d, 1e-9)
	require.InDelta(t, 3.5, data.Usage7d, 1e-9)
	require.Equal(t, dayStart, data.Window1d, "window starts are never moved by the cache")
	require.Equal(t, dayStart, data.Window7d)
}

func TestUpdateAPIKeyRateLimitUsage_MissingEntryIsNoop(t *testing.T) {
	ctx := context.Background()
	cache, server := newRateLimitWindowCache(t)
	require.NoError(t, cache.UpdateAPIKeyRateLimitUsage(ctx, 10, 1))
	require.False(t, server.Exists(billingRateLimitKey(10)))
}

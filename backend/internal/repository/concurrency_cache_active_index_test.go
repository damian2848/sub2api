package repository

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// roundTripCounter counts client round trips: one per command or pipeline.
type roundTripCounter struct{ n atomic.Int64 }

func (h *roundTripCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *roundTripCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.n.Add(1)
		return next(ctx, cmd)
	}
}

func (h *roundTripCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.n.Add(1)
		return next(ctx, cmds)
	}
}

type activeIndexFixture struct {
	server  *miniredis.Miniredis
	client  *redis.Client
	cache   *concurrencyCache
	counter *roundTripCounter
}

func newActiveIndexTestCache(t *testing.T) (*miniredis.Miniredis, *redis.Client, *concurrencyCache) {
	f := newActiveIndexFixture(t)
	return f.server, f.client, f.cache
}

func newActiveIndexFixture(t *testing.T) *activeIndexFixture {
	t.Helper()
	server := miniredis.RunT(t)
	server.SetTime(time.Unix(1_800_000_000, 0))
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	counter := &roundTripCounter{}
	client.AddHook(counter)
	t.Cleanup(func() { _ = client.Close() })
	// 1 minute slot TTL, 30s wait TTL.
	cache, ok := NewConcurrencyCache(client, 1, 30).(*concurrencyCache)
	require.True(t, ok)
	return &activeIndexFixture{server: server, client: client, cache: cache, counter: counter}
}

// roundTripsFor counts the client round trips one operation needs once its
// scripts are loaded (go-redis falls back from EVALSHA to EVAL only once).
func (f *activeIndexFixture) roundTripsFor(op func()) int {
	before := f.counter.n.Load()
	op()
	return int(f.counter.n.Load() - before)
}

func TestActiveIndex_AcquireMarksIndexInSameRoundTrip(t *testing.T) {
	f := newActiveIndexFixture(t)
	client, cache := f.client, f.cache
	ctx := context.Background()
	// Warm the script cache.
	ok, err := cache.AcquireAccountSlot(ctx, 1, 5, "warm")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.AcquireUserSlot(ctx, 1, 5, "warm")
	require.NoError(t, err)
	require.True(t, ok)

	n := f.roundTripsFor(func() {
		ok, err = cache.AcquireAccountSlot(ctx, 42, 2, "req-a")
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, n, "admission and index marking share one script round trip")
	score, err := client.ZScore(ctx, accountActiveIndexKey, "42").Result()
	require.NoError(t, err)
	require.Equal(t, float64(1_800_000_000+60), score)

	n = f.roundTripsFor(func() {
		ok, err = cache.AcquireUserSlot(ctx, 7, 2, "req-u")
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, n)
	score, err = client.ZScore(ctx, userActiveIndexKey, "7").Result()
	require.NoError(t, err)
	require.Equal(t, float64(1_800_000_000+60), score)
}

func TestActiveIndex_RejectedAcquireDoesNotMarkIndex(t *testing.T) {
	_, client, cache := newActiveIndexTestCache(t)
	ctx := context.Background()
	ok, err := cache.AcquireAccountSlot(ctx, 5, 0, "req")
	require.NoError(t, err)
	require.False(t, ok)
	_, err = client.ZScore(ctx, accountActiveIndexKey, "5").Result()
	require.ErrorIs(t, err, redis.Nil)
}

func TestActiveIndex_ReleaseAndDecrementAreSingleRoundTrip(t *testing.T) {
	f := newActiveIndexFixture(t)
	server, client, cache := f.server, f.client, f.cache
	ctx := context.Background()
	ok, err := cache.AcquireAccountSlot(ctx, 9, 2, "req")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.IncrementAccountWaitCount(ctx, 9, 3)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.IncrementWaitCount(ctx, 9, 3)
	require.NoError(t, err)
	require.True(t, ok)
	// Warm the decrement script.
	require.NoError(t, cache.DecrementWaitCount(ctx, 9))

	require.Equal(t, 1, f.roundTripsFor(func() {
		require.NoError(t, cache.ReleaseAccountSlot(ctx, 9, "req"))
	}))
	require.Equal(t, 1, f.roundTripsFor(func() {
		require.NoError(t, cache.DecrementAccountWaitCount(ctx, 9))
	}))
	waiting, err := cache.GetAccountWaitingCount(ctx, 9)
	require.NoError(t, err)
	require.Zero(t, waiting)
	cur, err := cache.GetAccountConcurrency(ctx, 9)
	require.NoError(t, err)
	require.Zero(t, cur)

	// The index entry is left to expire on its own score...
	_, err = client.ZScore(ctx, accountActiveIndexKey, "9").Result()
	require.NoError(t, err)
	// ...and the periodic reconcile drops it once due and idle.
	server.SetTime(time.Unix(1_800_000_000+61, 0))
	require.NoError(t, cache.CleanupExpiredAccountSlotKeys(ctx))
	_, err = client.ZScore(ctx, accountActiveIndexKey, "9").Result()
	require.ErrorIs(t, err, redis.Nil)
	_, err = client.ZScore(ctx, userActiveIndexKey, "9").Result()
	require.ErrorIs(t, err, redis.Nil)
}

func TestActiveIndex_WaitMarkNeverLowersSlotExpiry(t *testing.T) {
	_, client, cache := newActiveIndexTestCache(t)
	ctx := context.Background()
	ok, err := cache.AcquireAccountSlot(ctx, 3, 2, "req")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.IncrementAccountWaitCount(ctx, 3, 3)
	require.NoError(t, err)
	require.True(t, ok)
	score, err := client.ZScore(ctx, accountActiveIndexKey, "3").Result()
	require.NoError(t, err)
	require.Equal(t, float64(1_800_000_000+60), score, "the shorter wait expiry must not replace the slot expiry")

	// A wait-only entry is marked with the wait TTL.
	ok, err = cache.IncrementAccountWaitCount(ctx, 4, 3)
	require.NoError(t, err)
	require.True(t, ok)
	score, err = client.ZScore(ctx, accountActiveIndexKey, "4").Result()
	require.NoError(t, err)
	require.Equal(t, float64(1_800_000_000+30), score)
	ok, err = cache.IncrementWaitCount(ctx, 8, 3)
	require.NoError(t, err)
	require.True(t, ok)
	score, err = client.ZScore(ctx, userActiveIndexKey, "8").Result()
	require.NoError(t, err)
	require.Equal(t, float64(1_800_000_000+30), score)
}

func TestActiveIndex_ReconcileKeepsLoadedEntries(t *testing.T) {
	server, client, cache := newActiveIndexTestCache(t)
	ctx := context.Background()
	ok, err := cache.IncrementAccountWaitCount(ctx, 11, 3)
	require.NoError(t, err)
	require.True(t, ok)
	// The index entry comes due while the waiter is still queued (SetTime
	// moves Redis TIME without expiring the counter key).
	server.SetTime(time.Unix(1_800_000_000+31, 0))
	require.NoError(t, cache.CleanupExpiredAccountSlotKeys(ctx))
	score, err := client.ZScore(ctx, accountActiveIndexKey, strconv.Itoa(11)).Result()
	require.NoError(t, err, "a still-loaded entry is re-armed, not dropped")
	require.Equal(t, float64(1_800_000_000+31+30), score)
}

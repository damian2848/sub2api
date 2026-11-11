//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type outstandingRecordingCache struct {
	stubConcurrencyCacheForTest
	mu              sync.Mutex
	incrementErr    error
	accountReleases []string
	userReleases    []string
	accountDecr     map[int64]int
	userDecr        map[int64]int
}

func newOutstandingRecordingCache() *outstandingRecordingCache {
	return &outstandingRecordingCache{
		stubConcurrencyCacheForTest: stubConcurrencyCacheForTest{acquireResult: true, waitAllowed: true},
		accountDecr:                 map[int64]int{},
		userDecr:                    map[int64]int{},
	}
}

func (c *outstandingRecordingCache) ReleaseAccountSlot(_ context.Context, _ int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accountReleases = append(c.accountReleases, requestID)
	return nil
}

func (c *outstandingRecordingCache) ReleaseUserSlot(_ context.Context, _ int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userReleases = append(c.userReleases, requestID)
	return nil
}

func (c *outstandingRecordingCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	return c.incrementErr == nil, c.incrementErr
}

func (c *outstandingRecordingCache) IncrementWaitCount(context.Context, int64, int) (bool, error) {
	return c.incrementErr == nil, c.incrementErr
}

func (c *outstandingRecordingCache) DecrementAccountWaitCount(_ context.Context, id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accountDecr[id]++
	return nil
}

func (c *outstandingRecordingCache) DecrementWaitCount(_ context.Context, id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userDecr[id]++
	return nil
}

func TestReleaseOutstanding_ReleasesHeldStateExactlyOnce(t *testing.T) {
	cache := newOutstandingRecordingCache()
	svc := NewConcurrencyService(cache)
	ctx := context.Background()

	account, err := svc.AcquireAccountSlot(ctx, 11, 2)
	require.NoError(t, err)
	user, err := svc.AcquireUserSlot(ctx, 22, 2)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		ok, err := svc.IncrementAccountWaitCount(ctx, 11, 5)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := svc.IncrementWaitCount(ctx, 22, 5)
	require.NoError(t, err)
	require.True(t, ok)

	svc.ReleaseOutstanding(ctx)
	require.Equal(t, []string{account.RequestID}, cache.accountReleases)
	require.Equal(t, []string{user.RequestID}, cache.userReleases)
	require.Equal(t, map[int64]int{11: 2}, cache.accountDecr)
	require.Equal(t, map[int64]int{22: 1}, cache.userDecr)

	// Handlers that finish after the flush must not release or decrement again.
	account.ReleaseFunc()
	user.ReleaseFunc()
	svc.DecrementAccountWaitCount(ctx, 11)
	svc.DecrementAccountWaitCount(ctx, 11)
	svc.DecrementWaitCount(ctx, 22)
	require.Len(t, cache.accountReleases, 1)
	require.Len(t, cache.userReleases, 1)
	require.Equal(t, map[int64]int{11: 2}, cache.accountDecr)
	require.Equal(t, map[int64]int{22: 1}, cache.userDecr)
}

func TestReleaseOutstanding_SkipsStateReleasedByOwners(t *testing.T) {
	cache := newOutstandingRecordingCache()
	svc := NewConcurrencyService(cache)
	ctx := context.Background()

	account, err := svc.AcquireAccountSlot(ctx, 1, 1)
	require.NoError(t, err)
	_, err = svc.IncrementAccountWaitCount(ctx, 1, 5)
	require.NoError(t, err)
	svc.DecrementAccountWaitCount(ctx, 1)
	account.ReleaseFunc()
	account.ReleaseFunc() // repeated release stays a single exact delete

	svc.ReleaseOutstanding(ctx)
	require.Equal(t, []string{account.RequestID}, cache.accountReleases)
	require.Equal(t, map[int64]int{1: 1}, cache.accountDecr)
}

func TestReleaseOutstanding_FailOpenIncrementStaysPaired(t *testing.T) {
	cache := newOutstandingRecordingCache()
	cache.incrementErr = errors.New("reply lost")
	svc := NewConcurrencyService(cache)
	ctx := context.Background()

	ok, err := svc.IncrementAccountWaitCount(ctx, 3, 5)
	require.NoError(t, err)
	require.True(t, ok, "wait counting still fails open")
	ok, err = svc.IncrementAccountWaitCount(ctx, 4, 5)
	require.NoError(t, err)
	require.True(t, ok)

	// The handler's decrement is still sent for an ambiguous increment...
	svc.DecrementAccountWaitCount(ctx, 3)
	// ...and one the handler never got to is flushed at shutdown.
	svc.ReleaseOutstanding(ctx)
	require.Equal(t, map[int64]int{3: 1, 4: 1}, cache.accountDecr)
}

func TestReleaseOutstanding_UnpairedDecrementKeepsLegacyBehaviorUntilFlush(t *testing.T) {
	cache := newOutstandingRecordingCache()
	svc := NewConcurrencyService(cache)
	ctx := context.Background()

	svc.DecrementAccountWaitCount(ctx, 9)
	require.Equal(t, map[int64]int{9: 1}, cache.accountDecr, "an unpaired decrement is sent as before")
	svc.ReleaseOutstanding(ctx)
	svc.DecrementAccountWaitCount(ctx, 9)
	require.Equal(t, map[int64]int{9: 1}, cache.accountDecr, "after the flush nothing unowned is decremented")

	// State acquired after the flush is still owned and released normally.
	ok, err := svc.IncrementAccountWaitCount(ctx, 9, 5)
	require.NoError(t, err)
	require.True(t, ok)
	svc.DecrementAccountWaitCount(ctx, 9)
	require.Equal(t, map[int64]int{9: 2}, cache.accountDecr)
}

func TestReleaseOutstanding_ExpiredContextSendsNothing(t *testing.T) {
	cache := newOutstandingRecordingCache()
	svc := NewConcurrencyService(cache)
	_, err := svc.AcquireAccountSlot(context.Background(), 1, 1)
	require.NoError(t, err)
	_, err = svc.IncrementAccountWaitCount(context.Background(), 1, 5)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.ReleaseOutstanding(ctx)
	require.Empty(t, cache.accountReleases)
	require.Empty(t, cache.accountDecr)
}

func TestReleaseOutstanding_NilSafety(t *testing.T) {
	var nilSvc *ConcurrencyService
	nilSvc.ReleaseOutstanding(context.Background())
	(&ConcurrencyService{}).ReleaseOutstanding(context.Background())
}

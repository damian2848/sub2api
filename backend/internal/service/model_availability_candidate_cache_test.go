//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// candidateCountingRepo counts ListModelAvailabilityCandidates calls and can
// block each call until released.
type candidateCountingRepo struct {
	AccountRepository
	calls   atomic.Int64
	gate    chan struct{}
	err     error
	lastCtx atomic.Value
}

func (r *candidateCountingRepo) ListModelAvailabilityCandidates(ctx context.Context, _ *int64, _ []string, _ bool) ([]Account, error) {
	r.calls.Add(1)
	r.lastCtx.Store(ctx)
	if r.gate != nil {
		<-r.gate
	}
	if r.err != nil {
		return nil, r.err
	}
	return []Account{
		{ID: 1, Platform: PlatformAnthropic, Status: StatusActive, Schedulable: true},
		{ID: 2, Platform: PlatformAnthropic, Status: StatusActive, Schedulable: true},
	}, nil
}

func TestModelAvailabilityCandidateCache_ConcurrentMissesShareOneQuery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &candidateCountingRepo{gate: make(chan struct{})}
		var cache modelAvailabilityCandidateCache
		groupID := int64(7)

		const callers = 50
		var wg sync.WaitGroup
		results := make([][]Account, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				accounts, err := cache.list(context.Background(), repo, time.Second, &groupID, []string{PlatformAnthropic}, false)
				require.NoError(t, err)
				results[i] = accounts
			}(i)
		}
		synctest.Wait()
		close(repo.gate)
		wg.Wait()

		require.Equal(t, int64(1), repo.calls.Load(), "a storm of failed requests must share one repository query")
		for _, accounts := range results {
			require.Len(t, accounts, 2)
		}

		// Callers own their copies: a write to one must not reach the next reader.
		results[0][0].Status = StatusDisabled
		again, err := cache.list(context.Background(), repo, time.Second, &groupID, []string{PlatformAnthropic}, false)
		require.NoError(t, err)
		require.Equal(t, StatusActive, again[0].Status)
		require.Equal(t, int64(1), repo.calls.Load(), "a hit inside the TTL does not query")
	})
}

func TestModelAvailabilityCandidateCache_ExpiresAndKeysByScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &candidateCountingRepo{}
		var cache modelAvailabilityCandidateCache
		groupA, groupB := int64(1), int64(2)
		ttl := 1500 * time.Millisecond
		ctx := context.Background()

		_, err := cache.list(ctx, repo, ttl, &groupA, []string{PlatformAnthropic}, false)
		require.NoError(t, err)
		_, err = cache.list(ctx, repo, ttl, &groupA, []string{PlatformAnthropic}, false)
		require.NoError(t, err)
		require.Equal(t, int64(1), repo.calls.Load())

		// A different group, platform set or includeGrouped flag is a different scope.
		_, _ = cache.list(ctx, repo, ttl, &groupB, []string{PlatformAnthropic}, false)
		_, _ = cache.list(ctx, repo, ttl, &groupA, []string{PlatformAnthropic, PlatformGemini}, false)
		_, _ = cache.list(ctx, repo, ttl, &groupA, []string{PlatformAnthropic}, true)
		_, _ = cache.list(ctx, repo, ttl, nil, []string{PlatformAnthropic}, true)
		require.Equal(t, int64(5), repo.calls.Load())

		// Platform order does not split the scope.
		_, _ = cache.list(ctx, repo, ttl, &groupA, []string{PlatformGemini, PlatformAnthropic}, false)
		require.Equal(t, int64(5), repo.calls.Load())

		time.Sleep(ttl)
		_, err = cache.list(ctx, repo, ttl, &groupA, []string{PlatformAnthropic}, false)
		require.NoError(t, err)
		require.Equal(t, int64(6), repo.calls.Load(), "an expired entry is reloaded")
	})
}

func TestModelAvailabilityCandidateCache_ErrorsAreNotCached(t *testing.T) {
	repo := &candidateCountingRepo{err: errors.New("db down")}
	var cache modelAvailabilityCandidateCache
	groupID := int64(3)

	for i := 0; i < 3; i++ {
		_, err := cache.list(context.Background(), repo, time.Second, &groupID, []string{PlatformAnthropic}, false)
		require.EqualError(t, err, "db down")
	}
	require.Equal(t, int64(3), repo.calls.Load())

	repo.err = nil
	accounts, err := cache.list(context.Background(), repo, time.Second, &groupID, []string{PlatformAnthropic}, false)
	require.NoError(t, err)
	require.Len(t, accounts, 2)
}

func TestModelAvailabilityCandidateCache_ZeroTTLQueriesDirectly(t *testing.T) {
	repo := &candidateCountingRepo{}
	var cache modelAvailabilityCandidateCache
	groupID := int64(4)

	for i := 0; i < 3; i++ {
		_, err := cache.list(context.Background(), repo, 0, &groupID, []string{PlatformAnthropic}, false)
		require.NoError(t, err)
	}
	require.Equal(t, int64(3), repo.calls.Load())
	require.Empty(t, cache.entries)
}

func TestModelAvailabilityCandidateCache_CallerCancelDoesNotCancelSharedQuery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &candidateCountingRepo{gate: make(chan struct{})}
		var cache modelAvailabilityCandidateCache
		groupID := int64(5)

		leaderCtx, cancelLeader := context.WithCancel(context.Background())
		leaderErr := make(chan error, 1)
		go func() {
			_, err := cache.list(leaderCtx, repo, time.Second, &groupID, []string{PlatformAnthropic}, false)
			leaderErr <- err
		}()
		synctest.Wait() // the leader owns the shared query before the follower joins
		followerDone := make(chan []Account, 1)
		go func() {
			accounts, err := cache.list(context.Background(), repo, time.Second, &groupID, []string{PlatformAnthropic}, false)
			require.NoError(t, err)
			followerDone <- accounts
		}()
		synctest.Wait()

		cancelLeader()
		require.ErrorIs(t, <-leaderErr, context.Canceled, "the caller that left gets its own cancellation")

		loadCtx, _ := repo.lastCtx.Load().(context.Context)
		require.NoError(t, loadCtx.Err(), "the shared query is detached from the caller that started it")
		_, hasDeadline := loadCtx.Deadline()
		require.True(t, hasDeadline, "the shared query is still bounded")

		close(repo.gate)
		require.Len(t, <-followerDone, 2)
		require.Equal(t, int64(1), repo.calls.Load())
	})
}

func TestResolveModelAvailabilityDiagnosisCacheTTL(t *testing.T) {
	require.Zero(t, resolveModelAvailabilityDiagnosisCacheTTL(nil))
	cfg := &config.Config{}
	require.Zero(t, resolveModelAvailabilityDiagnosisCacheTTL(cfg))
	cfg.Gateway.ModelAvailabilityDiagnosisCacheTTLMillis = 1500
	require.Equal(t, 1500*time.Millisecond, resolveModelAvailabilityDiagnosisCacheTTL(cfg))
}

type candidatePanickingRepo struct {
	AccountRepository
}

func (candidatePanickingRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error) {
	panic("corrupt account row")
}

func TestModelAvailabilityCandidateCache_PanicBecomesError(t *testing.T) {
	var cache modelAvailabilityCandidateCache
	groupID := int64(6)
	accounts, err := cache.list(context.Background(), candidatePanickingRepo{}, time.Second, &groupID, []string{PlatformAnthropic}, false)
	require.ErrorContains(t, err, "model availability candidate load panic")
	require.Nil(t, accounts)
	require.Empty(t, cache.entries)
}

func TestModelAvailabilityCandidateCache_ExpiredEntryIsReleasedOnLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &candidateCountingRepo{}
		var cache modelAvailabilityCandidateCache
		groupID := int64(8)
		_, err := cache.list(context.Background(), repo, time.Second, &groupID, []string{PlatformAnthropic}, false)
		require.NoError(t, err)
		require.Len(t, cache.entries, 1)

		time.Sleep(time.Second)
		key := modelAvailabilityCandidateCacheKey(&groupID, []string{PlatformAnthropic}, false)
		_, ok := cache.lookup(key, time.Now())
		require.False(t, ok)
		require.Empty(t, cache.entries, "an expired entry must not keep the group's account rows alive")
	})
}

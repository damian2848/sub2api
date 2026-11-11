//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type schedulerEmptyBucketCache struct {
	SchedulerCache
	mu        sync.Mutex
	snapshot  []*Account
	readErr   error
	setCalls  int
	tokenCaps int
}

func (c *schedulerEmptyBucketCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return nil, false, c.readErr
	}
	if len(c.snapshot) == 0 {
		return nil, false, nil
	}
	return c.snapshot, true, nil
}

func (c *schedulerEmptyBucketCache) CaptureBucketWriteToken(context.Context, SchedulerBucket) (SchedulerBucketWriteToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokenCaps++
	return SchedulerBucketWriteToken{}, nil
}

func (c *schedulerEmptyBucketCache) SetSnapshot(context.Context, SchedulerBucket, SchedulerBucketWriteToken, []Account) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setCalls++
	return nil
}

type schedulerEmptyBucketRepo struct {
	AccountRepository
	calls    atomic.Int32
	release  chan struct{}
	entered  chan struct{}
	accounts []Account
	err      error
}

func (r *schedulerEmptyBucketRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, _ string) ([]Account, error) {
	if r.calls.Add(1) == 1 && r.entered != nil {
		close(r.entered)
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.accounts, nil
}

func newSchedulerEmptyBucketService(cache SchedulerCache, repo AccountRepository, now *time.Time) *SchedulerSnapshotService {
	svc := NewSchedulerSnapshotService(cache, nil, repo, nil, nil)
	svc.nowFn = func() time.Time { return *now }
	return svc
}

func TestSchedulerSnapshotEmptyBucketCoalescesAndNegativeCaches(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := &schedulerEmptyBucketCache{}
	repo := &schedulerEmptyBucketRepo{release: make(chan struct{}), entered: make(chan struct{})}
	svc := newSchedulerEmptyBucketService(cache, repo, &now)

	const callers = 100
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
			if err == nil && len(accounts) != 0 {
				err = errors.New("expected empty accounts")
			}
			errs <- err
		}()
	}
	<-repo.entered
	// 给其余 goroutine 时间加入同一个 flight；晚到的会命中负缓存，同样不查库。
	time.Sleep(50 * time.Millisecond)
	close(repo.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), repo.calls.Load(), "concurrent empty-bucket fallbacks must share one DB load")
	require.Equal(t, 1, cache.tokenCaps, "only the flight leader captures a publish token")
	require.Equal(t, 1, cache.setCalls, "only the flight leader publishes the snapshot")

	// 负缓存有效期内不再查库。
	accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.Equal(t, int32(1), repo.calls.Load())

	// 过期后重新回退一次 DB。
	now = now.Add(schedulerEmptyBucketTTL)
	accounts, _, err = svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.Equal(t, int32(2), repo.calls.Load())
}

func TestSchedulerSnapshotEmptyBucketNonEmptySnapshotWins(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := &schedulerEmptyBucketCache{}
	repo := &schedulerEmptyBucketRepo{}
	svc := newSchedulerEmptyBucketService(cache, repo, &now)

	_, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, int32(1), repo.calls.Load())

	// outbox 在负缓存有效期内发布了非空快照，必须立即可见。
	cache.mu.Lock()
	cache.snapshot = []*Account{{ID: 7, Platform: PlatformOpenAI}}
	cache.mu.Unlock()
	accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, int64(7), accounts[0].ID)
	require.Equal(t, int32(1), repo.calls.Load())
}

func TestSchedulerSnapshotEmptyBucketNotCachedOnDBErrorOrRedisError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	dbErr := errors.New("db down")
	cache := &schedulerEmptyBucketCache{}
	repo := &schedulerEmptyBucketRepo{err: dbErr}
	svc := newSchedulerEmptyBucketService(cache, repo, &now)

	_, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
	require.ErrorIs(t, err, dbErr)
	repo.err = nil
	_, _, err = svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, int32(2), repo.calls.Load(), "a DB error must not populate the negative cache")

	// Redis 读失败时既不写也不读负缓存，每次照常回退。
	svc2 := newSchedulerEmptyBucketService(&schedulerEmptyBucketCache{readErr: errors.New("redis down")}, repo, &now)
	for range 3 {
		_, _, err = svc2.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
		require.NoError(t, err)
	}
	require.Equal(t, int32(5), repo.calls.Load())
}

func TestSchedulerSnapshotFallbackFollowerHonorsOwnCancellation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache := &schedulerEmptyBucketCache{}
	repo := &schedulerEmptyBucketRepo{
		release:  make(chan struct{}),
		entered:  make(chan struct{}),
		accounts: []Account{{ID: 1, Platform: PlatformOpenAI}},
	}
	svc := newSchedulerEmptyBucketService(cache, repo, &now)

	type leaderResult struct {
		accounts []Account
		err      error
	}
	leaderDone := make(chan leaderResult, 1)
	go func() {
		accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
		leaderDone <- leaderResult{accounts: accounts, err: err}
	}()
	<-repo.entered

	ctx, cancel := context.WithCancel(context.Background())
	followerDone := make(chan error, 1)
	go func() {
		_, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
		followerDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	require.ErrorIs(t, <-followerDone, context.Canceled)

	close(repo.release)
	leader := <-leaderDone
	require.NoError(t, leader.err)
	accounts := leader.accounts
	require.Len(t, accounts, 1)
	require.Equal(t, int32(1), repo.calls.Load())

	// 每个调用方拿到独立的顶层切片。
	accounts[0].ID = 99
	again, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, int64(1), again[0].ID)
}

type schedulerPanickingRepo struct {
	AccountRepository
	calls atomic.Int32
}

func (r *schedulerPanickingRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]Account, error) {
	r.calls.Add(1)
	panic("corrupt account row")
}

// singleflight re-panics on a fresh goroutine when DoChan has waiters, which
// no request-level recovery can catch. The shared load must turn a panic into
// an error for the requests that waited on it.
func TestSchedulerSnapshotFallbackPanicBecomesError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	repo := &schedulerPanickingRepo{}
	svc := newSchedulerEmptyBucketService(&schedulerEmptyBucketCache{}, repo, &now)

	for range 2 {
		accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
		require.ErrorContains(t, err, "scheduler snapshot fallback panic")
		require.Nil(t, accounts)
	}
	require.Equal(t, int32(2), repo.calls.Load(), "a panicking load is not negative-cached")
}

// The empty-bucket window is counted from when the DB was queried, not from
// when the (possibly slow) query returned.
func TestSchedulerSnapshotEmptyBucketWindowStartsAtQuery(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	repo := &schedulerEmptyBucketRepo{}
	svc := newSchedulerEmptyBucketService(&schedulerEmptyBucketCache{}, repo, &now)
	queryStart := now
	var once sync.Once
	svc.nowFn = func() time.Time {
		current := now
		// The clock jumps while the query is in flight.
		once.Do(func() { now = now.Add(2 * schedulerEmptyBucketTTL) })
		return current
	}

	_, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, int32(1), repo.calls.Load())
	until, ok := svc.emptyBucketUntil.Load(SchedulerBucket{GroupID: 0, Platform: PlatformOpenAI, Mode: SchedulerModeSingle})
	if ok {
		require.Equal(t, queryStart.Add(schedulerEmptyBucketTTL), until)
	}

	// The window already elapsed during the slow query, so the next request reloads.
	_, _, err = svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, int32(2), repo.calls.Load())
}

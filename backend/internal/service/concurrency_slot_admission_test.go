//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// admissionRecordingCache records the exact member written by an admission and
// every exact release, so tests can assert that no member is orphaned.
type admissionRecordingCache struct {
	stubConcurrencyCacheForTest
	mu             sync.Mutex
	acquire        func(ctx context.Context) (bool, error)
	acquireCalls   int
	acquiredIDs    []string
	releasedIDs    []string
	userAcquired   []string
	userReleased   []string
	releaseFailFor int
}

func (c *admissionRecordingCache) AcquireAccountSlot(ctx context.Context, _ int64, _ int, requestID string) (bool, error) {
	c.mu.Lock()
	c.acquireCalls++
	c.acquiredIDs = append(c.acquiredIDs, requestID)
	acquire := c.acquire
	c.mu.Unlock()
	return acquire(ctx)
}

func (c *admissionRecordingCache) ReleaseAccountSlot(_ context.Context, _ int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releasedIDs = append(c.releasedIDs, requestID)
	if c.releaseFailFor > 0 {
		c.releaseFailFor--
		return errors.New("transient release failure")
	}
	return nil
}

func (c *admissionRecordingCache) AcquireUserSlot(ctx context.Context, _ int64, _ int, requestID string) (bool, error) {
	c.mu.Lock()
	c.acquireCalls++
	c.userAcquired = append(c.userAcquired, requestID)
	acquire := c.acquire
	c.mu.Unlock()
	return acquire(ctx)
}

func (c *admissionRecordingCache) ReleaseUserSlot(_ context.Context, _ int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userReleased = append(c.userReleased, requestID)
	return nil
}

func (c *admissionRecordingCache) snapshot() (acquired, released []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.acquiredIDs...), append([]string(nil), c.releasedIDs...)
}

func (c *admissionRecordingCache) userSnapshot() (acquired, released []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.userAcquired...), append([]string(nil), c.userReleased...)
}

// waitForSlotCompensation lets every background compensation in the synctest
// bubble run to completion (its retries and backoff are bounded by the budget).
func waitForSlotCompensation() {
	synctest.Wait()
	time.Sleep(slotCompensationBudget)
	synctest.Wait()
}

func TestAcquireAccountSlot_CacheErrorCompensatesExactMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &admissionRecordingCache{acquire: func(context.Context) (bool, error) {
			// The script ran but its reply was lost.
			return false, context.DeadlineExceeded
		}}
		svc := NewConcurrencyService(cache)

		result, err := svc.AcquireAccountSlot(context.Background(), 7, 3)
		require.ErrorIs(t, err, context.DeadlineExceeded, "the original error must be returned unchanged")
		require.Nil(t, result)

		waitForSlotCompensation()
		acquired, released := cache.snapshot()
		require.Len(t, acquired, 1)
		require.Equal(t, acquired, released, "the ambiguous member must be removed by its exact request ID")
	})
}

func TestAcquireUserSlot_CacheErrorCompensatesExactMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &admissionRecordingCache{acquire: func(context.Context) (bool, error) {
			return false, errors.New("i/o timeout")
		}}
		svc := NewConcurrencyService(cache)

		result, err := svc.AcquireUserSlot(context.Background(), 9, 2)
		require.EqualError(t, err, "i/o timeout")
		require.Nil(t, result)

		waitForSlotCompensation()
		acquired, released := cache.userSnapshot()
		require.Len(t, acquired, 1)
		require.Equal(t, acquired, released)
	})
}

func TestAcquireAccountSlot_CompensationRetriesTransientReleaseFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &admissionRecordingCache{
			acquire:        func(context.Context) (bool, error) { return false, errors.New("reply lost") },
			releaseFailFor: 2,
		}
		svc := NewConcurrencyService(cache)

		_, err := svc.AcquireAccountSlot(context.Background(), 1, 1)
		require.Error(t, err)
		waitForSlotCompensation()
		acquired, released := cache.snapshot()
		require.Len(t, released, 3, "two transient failures are retried within the bounded budget")
		for _, id := range released {
			require.Equal(t, acquired[0], id)
		}
	})
}

// blockingReleaseCache fails every admission fast (Redis refusing connections)
// and makes every release hang until its context ends (Redis stuck), the worst
// case for compensation latency.
type blockingReleaseCache struct {
	admissionRecordingCache
	releaseCalls int
}

func (c *blockingReleaseCache) ReleaseAccountSlot(ctx context.Context, _ int64, requestID string) error {
	c.mu.Lock()
	c.releaseCalls++
	c.releasedIDs = append(c.releasedIDs, requestID)
	c.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func TestAcquireAccountSlot_ErrorReturnsWithoutWaitingForCompensation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &blockingReleaseCache{}
		cache.acquire = func(context.Context) (bool, error) { return false, errors.New("connection refused") }
		svc := NewConcurrencyService(cache)

		start := time.Now()
		for i := 0; i < 20; i++ { // a scheduler loop walking 20 candidates
			result, err := svc.AcquireAccountSlot(context.Background(), int64(i), 1)
			require.EqualError(t, err, "connection refused", "the original error is returned unchanged")
			require.Nil(t, result)
		}
		require.Zero(t, time.Since(start), "admission errors must not wait for compensation on the caller goroutine")

		// Compensation still runs for every ambiguous member, in the background.
		synctest.Wait()
		acquired, released := cache.snapshot()
		require.Len(t, acquired, 20)
		require.ElementsMatch(t, acquired, released, "each ambiguous member gets its exact compensation")

		// Every compensation goroutine gives up within its budget; synctest.Test
		// would report a deadlock if any of them leaked.
		time.Sleep(slotCompensationBudget)
		synctest.Wait()
		cache.mu.Lock()
		calls := cache.releaseCalls
		cache.mu.Unlock()
		require.Equal(t, 20*2, calls, "two 1s attempts fit in the 2s budget per member")
		require.Equal(t, start.Add(slotCompensationBudget), time.Now())
	})
}

func TestAcquireAccountSlot_AdmissionIgnoresCallerDeadlineButCallerReturnsOnTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var opDeadline time.Time
		var opErrAtCallerDeadline error
		cache := &admissionRecordingCache{}
		cache.acquire = func(ctx context.Context) (bool, error) {
			opDeadline, _ = ctx.Deadline()
			// Redis executes the script, but the reply arrives after the
			// caller's wait slice ended.
			time.Sleep(300 * time.Millisecond)
			opErrAtCallerDeadline = ctx.Err()
			return true, nil
		}
		svc := NewConcurrencyService(cache)
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		result, err := svc.AcquireAccountSlot(ctx, 5, 1)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Nil(t, result)
		require.Equal(t, 100*time.Millisecond, time.Since(start), "the caller keeps its original cancellation latency")

		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, start.Add(slotAdmissionTimeout), opDeadline, "the script runs on its own short budget")
		require.NoError(t, opErrAtCallerDeadline, "the caller deadline must not interrupt the in-flight script")
		acquired, released := cache.snapshot()
		require.Len(t, acquired, 1)
		require.Equal(t, acquired, released, "a member admitted after the caller left is released exactly")
	})
}

func TestAcquireAccountSlot_ClientCancelReleasesLateAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &admissionRecordingCache{}
		cache.acquire = func(ctx context.Context) (bool, error) {
			time.Sleep(50 * time.Millisecond)
			return true, ctx.Err()
		}
		svc := NewConcurrencyService(cache)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(10*time.Millisecond, cancel)

		_, err := svc.AcquireAccountSlot(ctx, 5, 1)
		require.ErrorIs(t, err, context.Canceled)
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		acquired, released := cache.snapshot()
		require.Equal(t, acquired, released)
	})
}

func TestAcquireAccountSlot_LateRejectionNeedsNoCompensation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &admissionRecordingCache{}
		cache.acquire = func(context.Context) (bool, error) {
			time.Sleep(50 * time.Millisecond)
			return false, nil
		}
		svc := NewConcurrencyService(cache)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		_, err := svc.AcquireAccountSlot(ctx, 5, 1)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		_, released := cache.snapshot()
		require.Empty(t, released, "a rejected admission wrote nothing")
	})
}

func TestAcquireAccountSlot_CancelledContextSendsNoCommand(t *testing.T) {
	cache := &admissionRecordingCache{acquire: func(context.Context) (bool, error) { return true, nil }}
	svc := NewConcurrencyService(cache)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := svc.AcquireAccountSlot(ctx, 5, 1)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	require.Zero(t, cache.acquireCalls)
	_, released := cache.snapshot()
	require.Empty(t, released)
}

func TestAcquireAccountSlot_SuccessDoesNotCompensate(t *testing.T) {
	cache := &admissionRecordingCache{acquire: func(context.Context) (bool, error) { return true, nil }}
	svc := NewConcurrencyService(cache)

	result, err := svc.AcquireAccountSlot(context.Background(), 5, 1)
	require.NoError(t, err)
	require.True(t, result.Acquired)
	_, released := cache.snapshot()
	require.Empty(t, released)
	result.ReleaseFunc()
	acquired, released := cache.snapshot()
	require.Equal(t, acquired, released)
}

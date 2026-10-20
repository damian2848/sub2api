package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type geminiWaitCache struct {
	service.ConcurrencyCache
	mu                               sync.Mutex
	acquire                          func(context.Context, int64) (bool, error)
	denyWait                         bool
	increments, decrements, releases map[int64]int
}

func newGeminiWaitCache() *geminiWaitCache {
	return &geminiWaitCache{increments: map[int64]int{}, decrements: map[int64]int{}, releases: map[int64]int{}}
}

func (s *geminiWaitCache) AcquireAccountSlot(ctx context.Context, id int64, _ int, _ string) (bool, error) {
	if s.acquire != nil {
		return s.acquire(ctx, id)
	}
	return false, nil
}

func (s *geminiWaitCache) ReleaseAccountSlot(_ context.Context, id int64, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases[id]++
	return nil
}

func (s *geminiWaitCache) IncrementAccountWaitCount(_ context.Context, id int64, _ int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.denyWait {
		return false, nil
	}
	s.increments[id]++
	return true, nil
}

func (s *geminiWaitCache) DecrementAccountWaitCount(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decrements[id]++
	return nil
}

func geminiWaitingSelection(id int64) *service.AccountSelectionResult {
	return &service.AccountSelectionResult{
		Account:  &service.Account{ID: id, Platform: service.PlatformAntigravity},
		WaitPlan: &service.AccountWaitPlan{AccountID: id, MaxConcurrency: 1, MaxWaiting: 3, Timeout: 30 * time.Second},
	}
}

func TestGeminiAccountWait_AdmitsRecoveredAlternativeAndReleasesOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/v1beta/models/gemini:generateContent")
		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		cache := newGeminiWaitCache()
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
		start := time.Now()
		budget := newGeminiAdmissionBudget()
		streamStarted, probes := false, 0
		cache.acquire = func(_ context.Context, id int64) (bool, error) {
			return id == 2 && time.Since(start) >= 2*time.Second, nil
		}
		selection, err := acquireGeminiAccount(c, helper, geminiWaitingSelection(1), budget, false, &streamStarted,
			func(ctx context.Context, busyID int64) (*service.AccountSelectionResult, error) {
				require.Equal(t, int64(1), busyID)
				probes++
				release, acquired, err := helper.TryAcquireAccountSlot(ctx, 2, 1)
				if !acquired {
					return geminiWaitingSelection(2), err
				}
				return &service.AccountSelectionResult{Account: &service.Account{ID: 2}, Acquired: true, ReleaseFunc: release}, err
			})
		require.NoError(t, err)
		require.Equal(t, int64(2), selection.Account.ID)
		require.True(t, selection.Acquired)
		require.Equal(t, 2, probes)
		require.Equal(t, 2*time.Second, time.Since(start))
		require.Equal(t, map[int64]int{1: 1}, cache.increments)
		require.Equal(t, cache.increments, cache.decrements)
		require.Empty(t, cache.releases)
		release := wrapReleaseOnDone(ctx, selection.ReleaseFunc)
		cancel()
		synctest.Wait()
		release()
		require.Equal(t, map[int64]int{2: 1}, cache.releases)
	})
}

func TestGeminiAccountWait_BusyAlternativeDoesNotMoveQueueOrResetBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/")
		cache := newGeminiWaitCache()
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
		start := time.Now()
		budget := &geminiAdmissionBudget{deadline: start.Add(3 * time.Second)}
		cache.acquire = func(_ context.Context, id int64) (bool, error) {
			return id == 1 && time.Since(start) >= 1500*time.Millisecond, nil
		}
		streamStarted, probes := false, 0
		selection, err := acquireGeminiAccount(c, helper, geminiWaitingSelection(1), budget, false, &streamStarted,
			func(context.Context, int64) (*service.AccountSelectionResult, error) {
				probes++
				return geminiWaitingSelection(2), nil
			})
		require.NoError(t, err)
		require.Equal(t, int64(1), selection.Account.ID)
		require.False(t, selection.Acquired, "queued admission must retain the downstream sticky bind path")
		require.Positive(t, probes)
		selection.ReleaseFunc()
		_, err = acquireGeminiAccount(c, helper, geminiWaitingSelection(2), budget, false, &streamStarted, nil)
		var timeout *ConcurrencyError
		require.ErrorAs(t, err, &timeout)
		require.True(t, timeout.IsTimeout)
		require.Equal(t, 3*time.Second, time.Since(start), "the second account cannot start a new 30s wait")
		require.Equal(t, map[int64]int{1: 1, 2: 1}, cache.increments)
		require.Equal(t, cache.increments, cache.decrements)
		require.Equal(t, map[int64]int{1: 1}, cache.releases)
		require.NoError(t, c.Request.Context().Err())
		_, hasDeadline := c.Request.Context().Deadline()
		require.False(t, hasDeadline, "admission budget must not constrain a successful generation")
	})
}

func TestGeminiAccountWait_CacheDeadlineEndsOnlyCurrentSlice(t *testing.T) {
	for _, recoverAlternative := range []bool{false, true} {
		t.Run(map[bool]string{false: "total_budget", true: "recovered_alternative"}[recoverAlternative], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, _ := newHelperTestContext(http.MethodPost, "/")
				cache := newGeminiWaitCache()
				cache.acquire = func(ctx context.Context, _ int64) (bool, error) {
					<-ctx.Done()
					return false, ctx.Err()
				}
				helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
				start := time.Now()
				budget := &geminiAdmissionBudget{deadline: start.Add(3 * time.Second)}
				started, probes := false, 0
				selection, err := acquireGeminiAccount(c, helper, geminiWaitingSelection(1), budget, false, &started,
					func(context.Context, int64) (*service.AccountSelectionResult, error) {
						probes++
						if recoverAlternative && probes == 2 {
							return &service.AccountSelectionResult{Account: &service.Account{ID: 2}, Acquired: true}, nil
						}
						return geminiWaitingSelection(2), nil
					})
				if recoverAlternative {
					require.NoError(t, err)
					require.Equal(t, int64(2), selection.Account.ID)
					require.Equal(t, 2*time.Second, time.Since(start))
				} else {
					var timeout *ConcurrencyError
					require.ErrorAs(t, err, &timeout)
					require.True(t, timeout.IsTimeout)
					require.Equal(t, 3*time.Second, time.Since(start))
				}
				require.Equal(t, 2, probes, "a raw cache deadline must still allow alternative probes")
				require.Equal(t, map[int64]int{1: 1}, cache.increments)
				require.Equal(t, cache.increments, cache.decrements)
				require.NoError(t, c.Request.Context().Err())
			})
		})
	}
}

func TestGeminiAccountWait_CacheCancellationPreservesParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/")
		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		cache := newGeminiWaitCache()
		cache.acquire = func(ctx context.Context, _ int64) (bool, error) {
			<-ctx.Done()
			return false, ctx.Err()
		}
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
		time.AfterFunc(100*time.Millisecond, cancel)
		start, started := time.Now(), false
		_, err := acquireGeminiAccount(c, helper, geminiWaitingSelection(1), newGeminiAdmissionBudget(), false, &started,
			func(context.Context, int64) (*service.AccountSelectionResult, error) {
				t.Fatal("client cancellation must not trigger another selection")
				return nil, nil
			})
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 100*time.Millisecond, time.Since(start))
		require.Equal(t, map[int64]int{1: 1}, cache.increments)
		require.Equal(t, cache.increments, cache.decrements)
	})
}

func TestGeminiAccountWait_BindingAndCommittedResponsePreventReselection(t *testing.T) {
	for _, test := range []struct {
		name                    string
		bound, started, written bool
	}{
		{name: "cached_content", bound: true}, {name: "stream_started", started: true}, {name: "response_written", written: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, rec := newHelperTestContext(http.MethodPost, "/")
				if test.written {
					_, _ = c.Writer.WriteString("already started")
				}
				originalBody := rec.Body.String()
				cache := newGeminiWaitCache()
				helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, 100*time.Millisecond)
				budget := &geminiAdmissionBudget{deadline: time.Now().Add(2 * time.Second)}
				started := test.started
				_, err := acquireGeminiAccount(c, helper, geminiWaitingSelection(1), budget, test.bound, &started,
					func(context.Context, int64) (*service.AccountSelectionResult, error) {
						t.Fatal("must not cross the account binding or committed response")
						return nil, nil
					})
				var timeout *ConcurrencyError
				require.ErrorAs(t, err, &timeout)
				require.True(t, timeout.IsTimeout)
				require.Equal(t, originalBody, rec.Body.String())
				require.Equal(t, cache.increments, cache.decrements)
				require.NoError(t, c.Request.Context().Err())
			})
		})
	}
}

func TestGeminiAccountWait_ClientCancellationBalancesQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/")
		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		cache := newGeminiWaitCache()
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
		time.AfterFunc(100*time.Millisecond, cancel)
		started := false
		_, err := acquireGeminiAccount(c, helper, geminiWaitingSelection(1), newGeminiAdmissionBudget(), false, &started, nil)
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, map[int64]int{1: 1}, cache.increments)
		require.Equal(t, cache.increments, cache.decrements)
		require.Empty(t, cache.releases)
	})
}

func TestGeminiAccountWait_NonAntigravityRetainsConfiguredStickyWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/")
		cache := newGeminiWaitCache()
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
		start := time.Now()
		selection := geminiWaitingSelection(1)
		selection.Account.Platform = service.PlatformGemini
		selection.WaitPlan.Timeout = 120 * time.Second
		started := false
		budget := newGeminiAdmissionBudget()
		_, err := acquireGeminiAccount(c, helper, selection, budget, false, &started,
			func(context.Context, int64) (*service.AccountSelectionResult, error) {
				t.Fatal("other providers retain their sticky wait policy")
				return nil, nil
			})
		var timeout *ConcurrencyError
		require.ErrorAs(t, err, &timeout)
		require.True(t, timeout.IsTimeout)
		require.Equal(t, 120*time.Second, time.Since(start))
		require.True(t, budget.deadline.IsZero())
		require.Equal(t, cache.increments, cache.decrements)
	})
}

func TestGeminiAccountWait_QueueFullCanImmediatelyUseAnAcquiredAlternative(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/")
		cache := newGeminiWaitCache()
		cache.denyWait = true
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
		started := false
		alternative := &service.AccountSelectionResult{Account: &service.Account{ID: 2}, Acquired: true}
		selection, err := acquireGeminiAccount(c, helper, geminiWaitingSelection(1), newGeminiAdmissionBudget(), false, &started,
			func(context.Context, int64) (*service.AccountSelectionResult, error) { return alternative, nil })
		require.NoError(t, err)
		require.Same(t, alternative, selection)
		require.Empty(t, cache.increments)
		require.Empty(t, cache.decrements)
	})
}

func TestGeminiAccountWait_LateAcquisitionIsReleased(t *testing.T) {
	for _, duringProbe := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial_selection", true: "alternative_selection"}[duringProbe], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, _ := newHelperTestContext(http.MethodPost, "/")
				cache := newGeminiWaitCache()
				helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
				started, releases := false, 0
				late := &service.AccountSelectionResult{Account: &service.Account{ID: 2}, Acquired: true, ReleaseFunc: func() { releases++ }}
				budget := &geminiAdmissionBudget{deadline: time.Now()}
				selection := late
				if duringProbe {
					budget.deadline = time.Now().Add(2 * time.Second)
					selection = geminiWaitingSelection(1)
				}
				_, err := acquireGeminiAccount(c, helper, selection, budget, false, &started,
					func(context.Context, int64) (*service.AccountSelectionResult, error) {
						time.Sleep(2 * time.Second)
						return late, nil
					})
				require.Error(t, err)
				require.Equal(t, 1, releases)
				require.Equal(t, cache.increments, cache.decrements)
			})
		})
	}
}

func TestGeminiAdmissionFailure_LocalBudgetPreservesUpstream429(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, rec := newHelperTestContext(http.MethodPost, "/")
		budget := &geminiAdmissionBudget{deadline: time.Now().Add(250 * time.Millisecond)}
		ctx, cancel := budget.context(c)
		defer cancel()
		failure := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, ResponseBody: []byte(`{"error":{"message":"upstream quota exhausted"}}`), RetryableOnSameAccount: true}
		fs := NewFailoverState(2, false)
		require.Equal(t, FailoverCanceled, fs.HandleFailoverError(ctx, nil, 1, service.PlatformAntigravity, 3, failure))
		require.NoError(t, c.Request.Context().Err())
		h := &GatewayHandler{}
		h.handleGeminiAdmissionFailure(c, budget.err(c), failure, false)
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		require.Contains(t, rec.Body.String(), "Upstream rate limit exceeded")
		require.NotContains(t, rec.Body.String(), "Concurrency")
	})
}

func TestGeminiWebAccountWait_ReselectsAndSharesBudgetAcrossFailover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/")
		cache := newGeminiWaitCache()
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
		start := time.Now()
		budget := newGeminiAdmissionBudget()
		selection := geminiWaitingSelection(1)
		selection.Account.Platform, selection.Account.Type = service.PlatformGemini, service.AccountTypeGeminiWeb
		selection.WaitPlan.Timeout = 120 * time.Second
		started, probes, releases := false, 0, 0
		admitted, err := acquireGeminiAccount(c, helper, selection, budget, false, &started,
			func(context.Context, int64) (*service.AccountSelectionResult, error) {
				probes++
				if probes < 2 {
					return nil, service.ErrNoAvailableAccounts
				}
				return &service.AccountSelectionResult{Account: &service.Account{ID: 2, Platform: service.PlatformGemini, Type: service.AccountTypeGeminiWeb}, Acquired: true, ReleaseFunc: func() { releases++ }}, nil
			})
		require.NoError(t, err)
		require.Equal(t, int64(2), admitted.Account.ID)
		require.Equal(t, 2*time.Second, time.Since(start))
		resumeAdmission := budget.pauseForForward()
		time.Sleep(35 * time.Second)
		resumeAdmission(nil)
		require.NoError(t, budget.err(c), "a slow failed generation must leave the remaining queue allowance intact")
		admitted.ReleaseFunc()
		require.Equal(t, 1, releases)
		_, err = acquireGeminiAccount(c, helper, selection, budget, false, &started, nil)
		var timeout *ConcurrencyError
		require.ErrorAs(t, err, &timeout)
		require.True(t, timeout.IsTimeout)
		require.Equal(t, 35*time.Second+maxConcurrencyWait, time.Since(start), "Web failover must share one budget even with 120s sticky waits")
		require.Equal(t, map[int64]int{1: 2}, cache.increments)
		require.Equal(t, cache.increments, cache.decrements)
		require.NoError(t, c.Request.Context().Err())
	})
}

func TestGeminiAdmissionBudget_ForwardPausePreservesAntigravityClock(t *testing.T) {
	for _, fromWeb := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "alternative_from_web"}[fromWeb], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, _ := newHelperTestContext(http.MethodPost, "/")
				budget := newGeminiAdmissionBudget()
				antigravity := &service.Account{ID: 2, Platform: service.PlatformAntigravity}
				if fromWeb {
					cache := newGeminiWaitCache()
					cache.denyWait = true
					helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
					selection := geminiWaitingSelection(1)
					selection.Account.Platform, selection.Account.Type = service.PlatformGemini, service.AccountTypeGeminiWeb
					started := false
					_, err := acquireGeminiAccount(c, helper, selection, budget, false, &started,
						func(context.Context, int64) (*service.AccountSelectionResult, error) {
							return &service.AccountSelectionResult{Account: antigravity, Acquired: true}, nil
						})
					require.NoError(t, err)
				} else {
					budget.observeAccount(antigravity)
				}
				resumeAdmission := budget.pauseForForward()
				time.Sleep(35 * time.Second)
				resumeAdmission(nil)
				var timeout *ConcurrencyError
				require.ErrorAs(t, budget.err(c), &timeout)
				require.True(t, timeout.IsTimeout, "the existing Antigravity admission/retry clock must not change")
			})
		})
	}
}

func TestGeminiWebAdmissionBudget_PrecheckRejectionsConsumeBudget(t *testing.T) {
	for _, reason := range []service.GatewayFailureReason{service.GeminiWebHealthCooldownReason, service.GeminiWebQuotaPrecheckReason} {
		t.Run(string(reason), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, _ := newHelperTestContext(http.MethodPost, "/")
				budget := newGeminiAdmissionBudget()
				budget.observeAccount(&service.Account{Platform: service.PlatformGemini, Type: service.AccountTypeGeminiWeb})
				for range 10 {
					resumeAdmission := budget.pauseForForward()
					time.Sleep(3 * time.Second)
					resumeAdmission(&service.UpstreamFailoverError{Reason: reason})
				}
				var timeout *ConcurrencyError
				require.ErrorAs(t, budget.err(c), &timeout)
				require.True(t, timeout.IsTimeout, "precheck time must not create an unbounded admission loop")
			})
		})
	}
	require.False(t, isGeminiWebAdmissionRejection(&service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, Reason: "gemini_web"}), "a real generation failure still consumes the failover allowance")
	require.False(t, isGeminiWebAdmissionRejection(&service.UpstreamFailoverError{StatusCode: http.StatusUnauthorized, Reason: service.GeminiWebCredentialRejectedReason}))
}

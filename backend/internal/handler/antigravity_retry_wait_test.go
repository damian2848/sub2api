//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type retryWaitCache struct {
	fakeConcurrencyCache
	first                               int64
	waited, released, queueIn, queueOut int
}

func (s *retryWaitCache) AcquireAccountSlot(_ context.Context, id int64, _ int, _ string) (bool, error) {
	if s.first == 0 {
		s.first = id
		return true, nil
	}
	s.waited++
	return false, nil
}

func (s *retryWaitCache) ReleaseAccountSlot(context.Context, int64, string) error {
	s.released++
	return nil
}

func (s *retryWaitCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	s.queueIn++
	return true, nil
}

func (s *retryWaitCache) DecrementAccountWaitCount(context.Context, int64) error {
	s.queueOut++
	return nil
}

type retryWaitUpstream struct {
	service.HTTPUpstream
	calls int
}

func (s *retryWaitUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	s.calls++
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"Resource has been exhausted"}}`))}, nil
}

func TestAntigravityRetryWait_AllCompatibilityRoutesPreserveFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, platform, path, model, body string
		call                              func(*GatewayHandler, *gin.Context)
	}{
		{"messages-gemini", service.PlatformGemini, "/v1/messages", "gemini-4-argon", `{"model":"gemini-4-argon","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`, (*GatewayHandler).Messages},
		{"messages-claude", service.PlatformAntigravity, "/v1/messages", "claude-sonnet-4-5", `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`, (*GatewayHandler).Messages},
		{"chat-completions", service.PlatformAntigravity, "/v1/chat/completions", "gemini-4-argon", `{"model":"gemini-4-argon","stream":true,"messages":[{"role":"user","content":"hello"}]}`, (*GatewayHandler).ChatCompletions},
		{"responses", service.PlatformAntigravity, "/v1/responses", "gemini-4-argon", `{"model":"gemini-4-argon","stream":true,"input":"hello"}`, (*GatewayHandler).Responses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(52)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: tc.platform, Status: service.StatusActive}
			var accounts []*service.Account
			for _, id := range []int64{288, 296} {
				accounts = append(accounts, &service.Account{
					ID: id, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
					Status: service.StatusActive, Schedulable: true, Concurrency: 4,
					AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}},
					Credentials:   map[string]any{"access_token": "test-token", "project_id": "test-project", "model_mapping": map[string]any{tc.model: tc.model}},
					Extra:         map[string]any{"mixed_scheduling": true},
				})
			}
			h, cleanup := newTestGatewayHandler(t, group, accounts)
			defer cleanup()
			cache := &retryWaitCache{}
			concurrency := service.NewConcurrencyService(cache)
			h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatClaude, time.Second)
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Gateway.Scheduling.FallbackWaitTimeout = 120 * time.Second
			cfg.Gateway.Scheduling.FallbackMaxWaiting = 10
			snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
			h.gatewayService = service.NewGatewayService(
				nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, cfg,
				snapshot, concurrency, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			)
			upstream := &retryWaitUpstream{}
			h.antigravityGatewayService = service.NewAntigravityGatewayService(nil, nil, nil, service.NewAntigravityTokenProvider(nil, nil, nil), nil, upstream, nil, nil)
			synctest.Test(t, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body)).WithContext(context.WithValue(context.Background(), ctxkey.Group, group))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, UserID: 2, GroupID: &groupID, Group: group, Status: service.StatusActive, User: &service.User{ID: 2, Concurrency: 10, Balance: 100}})
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 10})
				start := time.Now()
				tc.call(h, c)
				require.Equal(t, http.StatusTooManyRequests, recorder.Code, recorder.Body.String())
				require.NotContains(t, recorder.Body.String(), "concurrency")
				require.NotContains(t, recorder.Body.String(), string(SSEPingFormatClaude))
				require.Equal(t, 15*time.Second, time.Since(start), "retry queue must share the first account's deadline")
				require.NoError(t, c.Request.Context().Err(), "local retry exhaustion is not a 499")
				require.Equal(t, 1, upstream.calls)
				require.Positive(t, cache.waited)
				require.Equal(t, 1, cache.released, "first attempt's account slot is released once")
				require.Equal(t, cache.queueIn, cache.queueOut, "wait reservations are released on retry exhaustion")
			})
		})
	}
}

func TestAntigravityRetryWait_AdmissionDoesNotCancelGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/")
		cache := &helperConcurrencyCacheStub{accountSeq: []bool{true}}
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, time.Second)
		started := false
		release, err := helper.acquireAccountSlotBeforeRetryDeadline(c, 1, 1, 120*time.Second, &started, time.Now().Add(time.Second))
		require.NoError(t, err)
		time.Sleep(2 * time.Second)
		require.NoError(t, c.Request.Context().Err())
		require.Zero(t, cache.accountReleaseCalls, "retry deadline must not release an active generation's slot")
		release = wrapReleaseOnDone(c.Request.Context(), release)
		release()
		release()
		require.Equal(t, 1, cache.accountReleaseCalls)
	})
}

func TestAntigravityRetryWait_NonAntigravityKeepsConfiguredWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, recorder := newHelperTestContext(http.MethodPost, "/")
		cache := &helperConcurrencyCacheStub{}
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, time.Second)
		started := false
		start := time.Now()
		_, err := helper.AcquireAccountSlotWithRetryBudget(c, 1, 1, 120*time.Second, true, &started)
		var timeout *ConcurrencyError
		require.ErrorAs(t, err, &timeout)
		require.True(t, timeout.IsTimeout)
		require.Equal(t, 120*time.Second, time.Since(start))
		require.True(t, started)
		require.Contains(t, recorder.Body.String(), string(SSEPingFormatClaude), "other providers retain existing queue heartbeat behavior")
	})
}

func TestAntigravityRetryWait_ShorterQueuePolicyRemainsConcurrencyTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := newHelperTestContext(http.MethodPost, "/")
		helper := NewConcurrencyHelper(service.NewConcurrencyService(&helperConcurrencyCacheStub{}), SSEPingFormatNone, 0)
		started := false
		start := time.Now()
		_, err := helper.acquireAccountSlotBeforeRetryDeadline(c, 1, 1, time.Second, &started, start.Add(15*time.Second))
		var timeout *ConcurrencyError
		require.ErrorAs(t, err, &timeout)
		require.True(t, timeout.IsTimeout)
		require.Equal(t, time.Second, time.Since(start))
		require.NotErrorIs(t, err, errAntigravityRetryWaitExhausted)
	})
}

func TestAntigravityRetryWait_ClientCancellationAndLateAcquisition(t *testing.T) {
	for _, acquired := range []bool{false, true} {
		t.Run(map[bool]string{false: "client_cancel", true: "late_acquisition"}[acquired], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, _ := newHelperTestContext(http.MethodPost, "/")
				ctx, cancel := context.WithCancel(c.Request.Context())
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				cache := newGeminiWaitCache()
				cache.acquire = func(context.Context, int64) (bool, error) {
					if acquired {
						time.Sleep(2 * time.Second)
						return true, nil
					}
					return false, nil
				}
				if !acquired {
					time.AfterFunc(100*time.Millisecond, cancel)
				}
				helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, 0)
				started := false
				_, err := helper.acquireAccountSlotBeforeRetryDeadline(c, 1, 1, 120*time.Second, &started, time.Now().Add(time.Second))
				if acquired {
					require.ErrorIs(t, err, errAntigravityRetryWaitExhausted)
					require.Equal(t, 1, cache.releases[1])
					require.NoError(t, ctx.Err())
				} else {
					require.ErrorIs(t, err, context.Canceled)
					require.Empty(t, cache.releases)
				}
			})
		})
	}
}

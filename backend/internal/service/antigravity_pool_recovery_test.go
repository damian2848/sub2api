//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const genericPool429Body = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

func TestAntigravityPoolRecoveryGeneric429Scope(t *testing.T) {
	t.Setenv(antigravityPoolRecoveryGroupsEnv, "55")
	for _, tc := range []struct {
		name, body, model, platform, retryAfter string
		groupID                                 int64
		status                                  int
		wantStop                                bool
	}{
		{name: "generic exhaustion", body: genericPool429Body, groupID: 55, status: 429, wantStop: true},
		{name: "other group", body: genericPool429Body, groupID: 56, status: 429},
		{name: "other model family", body: genericPool429Body, groupID: 55, model: "claude-opus", status: 429},
		{name: "other platform", body: genericPool429Body, groupID: 55, platform: PlatformGemini, status: 429},
		{name: "explicit backoff", body: genericPool429Body, groupID: 55, retryAfter: "30", status: 429},
		{name: "quota metadata", body: `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED","details":[{"metadata":{"quotaResetDelay":"24h"}}]}}`, groupID: 55, status: 429},
		{name: "different error", body: `{"error":{"code":429,"message":"QUOTA_EXHAUSTED","status":"RESOURCE_EXHAUSTED"}}`, groupID: 55, status: 429},
		{name: "ordinary 503", body: genericPool429Body, groupID: 55, status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.model == "" {
				tc.model = "gemini-3.8-flash-high"
			}
			if tc.platform == "" {
				tc.platform = PlatformAntigravity
			}
			ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{ID: tc.groupID})
			p := antigravityRetryLoopParams{ctx: ctx, account: &Account{ID: 1, Platform: tc.platform}, requestedModel: tc.model,
				retryBudget: &antigravityRetryBudget{deadline: time.Now().Add(time.Minute)}}
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{"X-Request-Id": {"synthetic"}}}
			if tc.retryAfter != "" {
				resp.Header.Set("Retry-After", tc.retryAfter)
			}
			p.retryBudget.recordResponse(resp, []byte(tc.body))
			require.Nil(t, p.repeatedGeneric429Error(resp), "one account may fail without disabling the pool")
			require.Nil(t, p.repeatedGeneric429Error(resp), "repeated failure of the same account is not two accounts")
			p.account = &Account{ID: 2, Platform: tc.platform}
			failure := p.repeatedGeneric429Error(resp)
			if !tc.wantStop {
				require.Nil(t, failure)
				return
			}
			require.NotNil(t, failure)
			require.False(t, failure.ShouldRetryNextAccount())
			require.Equal(t, 429, failure.StatusCode)
			require.Equal(t, tc.body, string(failure.ResponseBody))
			require.Equal(t, "synthetic", failure.ResponseHeaders.Get("X-Request-Id"))
			p.retryBudget.recordResponse(&http.Response{StatusCode: 503}, []byte(`{"error":{"message":"Busy"}}`))
			p.retryBudget.recordResponse(resp, []byte(tc.body))
			require.Nil(t, p.repeatedGeneric429Error(resp), "a different intervening error breaks the sequence")
		})
	}
}

func TestAntigravityPoolRecoveryRetryLoopStopsAfterTwoAccounts(t *testing.T) {
	t.Setenv(antigravityPoolRecoveryGroupsEnv, "55")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	var attempts, cooled []int64
	p := antigravityRetryLoopParams{ctx: context.Background(), c: c, groupID: 55, requestedModel: "gemini-3.8-flash-high",
		action: "generateContent", body: []byte(`{}`),
		handleError: func(_ context.Context, _ string, a *Account, _ int, _ http.Header, _ []byte, _ string, _ int64, _ string, _ bool) *handleModelRateLimitResult {
			cooled = append(cooled, a.ID)
			return nil
		}}
	svc := &AntigravityGatewayService{}
	for _, id := range []int64{1, 2, 3, 4} {
		p.account = &Account{ID: id, Platform: PlatformAntigravity}
		p.httpUpstream = &antigravityBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
			attempts = append(attempts, id)
			return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(genericPool429Body))}, nil
		}}
		result, err := svc.antigravityRetryLoop(p)
		if err != nil {
			failure := antigravityTerminalRetryError(err)
			require.NotNil(t, failure)
			require.Equal(t, genericPool429Body, string(failure.ResponseBody))
			break
		}
		require.NoError(t, result.resp.Body.Close())
	}
	require.Equal(t, []int64{1, 2}, attempts)
	require.Equal(t, attempts, cooled, "only accounts actually rejected by upstream are cooled")
}

func newPoolRecoveryTestGateway(resetAt time.Time) (*GatewayService, context.Context, int64) {
	groupID := int64(55)
	group := &Group{ID: groupID, Hydrated: true, Platform: PlatformAntigravity, Status: StatusActive}
	a := Account{ID: 331, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 20,
		AccountGroups: []AccountGroup{{GroupID: groupID}},
		Credentials:   map[string]any{"model_mapping": map[string]any{"gemini-3.8-flash-high": "gemini-3.8-flash-high"}},
		Extra:         map[string]any{"model_rate_limits": map[string]any{"gemini-3.8-flash-high": map[string]any{"rate_limit_reset_at": resetAt.UTC().Format(time.RFC3339Nano)}}}}
	repo := &mockAccountRepoForPlatform{accounts: []Account{a}, accountsByID: map[int64]*Account{a.ID: &a}}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &GatewayService{accountRepo: repo, groupRepo: &mockGroupRepoForGateway{groups: map[int64]*Group{groupID: group}}, cfg: cfg}
	return svc, context.WithValue(context.Background(), ctxkey.Group, group), groupID
}

func TestAntigravityPoolRecoveryWaitAndReselect(t *testing.T) {
	t.Setenv(antigravityPoolRecoveryGroupsEnv, "55")
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		svc, ctx, groupID := newPoolRecoveryTestGateway(start.Add(2 * time.Second))
		selection, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", "gemini-3.8-flash-high", nil, "", 0)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Equal(t, int64(331), selection.Account.ID)
		require.GreaterOrEqual(t, time.Since(start), 2*time.Second)
		require.Less(t, time.Since(start), 3*time.Second)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		_, hasDeadline := ctx.Deadline()
		require.False(t, hasDeadline, "admission delay must not impose a deadline on generation")
	})
}

func TestAntigravityPoolRecoveryWaitBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		enabled  bool
		cooldown time.Duration
		exclude  bool
		deadline time.Duration
	}{
		{name: "disabled by default", cooldown: time.Second},
		{name: "long cooldown", enabled: true, cooldown: time.Minute},
		{name: "already tried an account", enabled: true, cooldown: time.Second, exclude: true},
		{name: "caller deadline", enabled: true, cooldown: 2 * time.Second, deadline: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := ""
			if tc.enabled {
				value = "55"
			}
			t.Setenv(antigravityPoolRecoveryGroupsEnv, value)
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				svc, ctx, groupID := newPoolRecoveryTestGateway(start.Add(tc.cooldown))
				if tc.deadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.deadline)
					defer cancel()
				}
				var excluded map[int64]struct{}
				if tc.exclude {
					excluded = map[int64]struct{}{999: {}}
				}
				selection, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", "gemini-3.8-flash-high", excluded, "", 0)
				require.Nil(t, selection)
				require.ErrorIs(t, err, ErrNoAvailableAccounts)
				require.Zero(t, time.Since(start))
			})
		})
	}
}

func TestAntigravityPoolRecoveryCancellationReleasesWaiter(t *testing.T) {
	t.Setenv(antigravityPoolRecoveryGroupsEnv, "55")
	synctest.Test(t, func(t *testing.T) {
		svc, ctx, groupID := newPoolRecoveryTestGateway(time.Now().Add(2 * time.Second))
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() { time.Sleep(time.Second); cancel() }()
		selection, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", "gemini-3.8-flash-high", nil, "", 0)
		require.Nil(t, selection)
		require.ErrorIs(t, err, context.Canceled)
		antigravityPoolWaiters.Lock()
		defer antigravityPoolWaiters.Unlock()
		require.Zero(t, antigravityPoolWaiters.total)
		require.Empty(t, antigravityPoolWaiters.counts)
	})
}

func TestAntigravityPoolRecoveryQueueBound(t *testing.T) {
	t.Setenv(antigravityPoolRecoveryGroupsEnv, "55")
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		results := make(chan error, 64)
		for range 64 {
			svc, groupCtx, groupID := newPoolRecoveryTestGateway(time.Now().Add(2 * time.Second))
			group := groupCtx.Value(ctxkey.Group)
			go func() {
				_, err := svc.SelectAccountWithLoadAwareness(context.WithValue(ctx, ctxkey.Group, group), &groupID, "", "gemini-3.8-flash-high", nil, "", 0)
				results <- err
			}()
		}
		synctest.Wait()
		svc, groupCtx, groupID := newPoolRecoveryTestGateway(time.Now().Add(2 * time.Second))
		start := time.Now()
		selection, err := svc.SelectAccountWithLoadAwareness(groupCtx, &groupID, "", "gemini-3.8-flash-high", nil, "", 0)
		require.Nil(t, selection)
		require.ErrorIs(t, err, ErrNoAvailableAccounts)
		require.Zero(t, time.Since(start), "overflow cannot create an unbounded waiter")
		cancel()
		for range 64 {
			require.ErrorIs(t, <-results, context.Canceled)
		}
		antigravityPoolWaiters.Lock()
		defer antigravityPoolWaiters.Unlock()
		require.Zero(t, antigravityPoolWaiters.total)
		require.Empty(t, antigravityPoolWaiters.counts)
	})
}

func TestAntigravityPoolRecoverySecondAccountCanSucceed(t *testing.T) {
	t.Setenv(antigravityPoolRecoveryGroupsEnv, "55")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	p := antigravityRetryLoopParams{ctx: context.Background(), c: c, groupID: 55, requestedModel: "gemini-3.8-flash-high", action: "generateContent", body: []byte(`{}`),
		handleError: func(context.Context, string, *Account, int, http.Header, []byte, string, int64, string, bool) *handleModelRateLimitResult {
			return nil
		}}
	svc := &AntigravityGatewayService{}
	for i, status := range []int{429, 200} {
		p.account = &Account{ID: int64(i + 1), Platform: PlatformAntigravity}
		p.httpUpstream = &antigravityBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
			body := genericPool429Body
			if status == 200 {
				body = `{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}}
		result, err := svc.antigravityRetryLoop(p)
		require.NoError(t, err)
		require.Equal(t, status, result.resp.StatusCode)
		require.NoError(t, result.resp.Body.Close())
	}
}

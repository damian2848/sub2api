//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type antigravityBudgetUpstream struct {
	do func(*http.Request) (*http.Response, error)
}

func (u *antigravityBudgetUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.do(req)
}

func (u *antigravityBudgetUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.do(req)
}

func TestAntigravityRetryBudgetQuotaDoesNotRetryShortDelay(t *testing.T) {
	for _, tc := range []struct {
		name, message, metadata string
		minimumCooldown         time.Duration
		retryFirst              bool
	}{
		{"explicit quota", "QUOTA_EXHAUSTED", `"model":"gemini-3-flash"`, 20 * time.Second, false},
		{"long reset", "Rate limit", `"model":"gemini-3-flash","quotaResetDelay":"24h"`, 23 * time.Hour, false},
		{"long reset after retry info", "Rate limit", `"model":"gemini-3-flash","quotaResetDelay":"24h"`, 23 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errorInfo := `{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{` + tc.metadata + `}}`
			retryInfo := `{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"1s"}`
			details := errorInfo + "," + retryInfo
			if tc.retryFirst {
				details = retryInfo + "," + errorInfo
			}
			body := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"` + tc.message + `","details":[` + details + `]}}`)
			repo := &stubAntigravityAccountRepo{}
			upstream := &recordingOKUpstream{}
			svc := &AntigravityGatewayService{}
			result := svc.handleSmartRetry(antigravityRetryLoopParams{
				ctx: context.Background(), account: &Account{ID: 51, Platform: PlatformAntigravity},
				accountRepo: repo, httpUpstream: upstream, requestedModel: "gemini-3-flash",
			}, &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}, body, "https://example.test", 0, []string{"https://example.test"})
			require.NotNil(t, result.switchError)
			require.Zero(t, upstream.calls, "quota exhaustion cannot be repaired by repeating the same request")
			require.NotEmpty(t, repo.modelRateLimitCalls)
			require.Greater(t, time.Until(repo.modelRateLimitCalls[0].resetAt), tc.minimumCooldown)
		})
	}
}

func TestAntigravityRetryBudgetStopsCapacityRetryAndKeepsUpstreamError(t *testing.T) {
	model := "budget-capacity-model"
	t.Cleanup(func() {
		modelCapacityExhaustedMu.Lock()
		delete(modelCapacityExhaustedUntil, model)
		modelCapacityExhaustedMu.Unlock()
	})
	body := []byte(`{"error":{"status":"UNAVAILABLE","message":"No model capacity","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED","metadata":{"model":"` + model + `"}}]}}`)
	upstream := &recordingOKUpstream{}
	svc := &AntigravityGatewayService{}
	result := svc.handleSmartRetry(antigravityRetryLoopParams{
		ctx: context.Background(), account: &Account{ID: 52, Platform: PlatformAntigravity},
		httpUpstream: upstream, retryBudget: &antigravityRetryBudget{deadline: time.Now().Add(50 * time.Millisecond)},
	}, &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"X-Request-Id": {"capacity-request"}}}, body, "https://example.test", 0, []string{"https://example.test"})
	failure := antigravityTerminalRetryError(result.err)
	require.NotNil(t, failure)
	require.False(t, failure.ShouldRetryNextAccount())
	require.Equal(t, http.StatusServiceUnavailable, failure.StatusCode)
	require.Equal(t, body, failure.ResponseBody)
	require.Equal(t, "capacity-request", failure.ResponseHeaders.Get("X-Request-Id"))
	require.Zero(t, upstream.calls, "a wait that exceeds remaining retry time must not start another attempt")
}

func TestAntigravityRetryBudgetSharedAcrossAccounts(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	body := `{"error":{"code":429,"message":"Resource has been exhausted"}}`
	upstream := &antigravityBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"30"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}}
	params := antigravityRetryLoopParams{
		ctx: context.Background(), c: c, account: &Account{ID: 53, Platform: PlatformAntigravity},
		httpUpstream: upstream, action: "generateContent", body: []byte(`{}`),
		handleError: func(context.Context, string, *Account, int, http.Header, []byte, string, int64, string, bool) *handleModelRateLimitResult {
			return nil
		},
	}
	svc := &AntigravityGatewayService{}
	first, err := svc.antigravityRetryLoop(params)
	require.NoError(t, err)
	_ = first.resp.Body.Close()
	deadline, ok := AntigravityRetryDeadline(c)
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(antigravityRetryTimeBudget), deadline, time.Second)
	value, _ := c.Get(antigravityRetryBudgetKey)
	value.(*antigravityRetryBudget).deadline = time.Now().Add(-time.Second)
	secondUpstream := &recordingOKUpstream{}
	params.account = &Account{ID: 54, Platform: PlatformAntigravity}
	params.httpUpstream = secondUpstream
	_, err = svc.antigravityRetryLoop(params)
	failure := antigravityTerminalRetryError(err)
	require.NotNil(t, failure)
	require.Equal(t, http.StatusTooManyRequests, failure.StatusCode)
	require.Equal(t, body, string(failure.ResponseBody))
	require.Equal(t, "30", failure.ResponseHeaders.Get("Retry-After"))
	require.Zero(t, secondUpstream.calls)
}

func TestAntigravityRetryBudgetNeverCancelsSuccessfulGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		var requestContext context.Context
		upstream := &antigravityBudgetUpstream{do: func(req *http.Request) (*http.Response, error) {
			requestContext = req.Context()
			_, hasDeadline := req.Context().Deadline()
			require.False(t, hasDeadline, "retry scheduling must not impose a generation deadline")
			time.Sleep(30 * time.Millisecond)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("complete stream after retry deadline"))}, nil
		}}
		svc := &AntigravityGatewayService{}
		result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
			ctx: ctx, account: &Account{ID: 55, Platform: PlatformAntigravity}, httpUpstream: upstream,
			action: "streamGenerateContent", body: []byte(`{}`), retryBudget: &antigravityRetryBudget{deadline: time.Now().Add(10 * time.Millisecond)},
		})
		require.NoError(t, err)
		require.NoError(t, requestContext.Err())
		data, err := io.ReadAll(result.resp.Body)
		require.NoError(t, err)
		require.Equal(t, "complete stream after retry deadline", string(data))
		_ = result.resp.Body.Close()
	})
}

func TestAntigravityRetryBudgetSlowFailureDoesNotStartAnotherAttempt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{"http failure", http.StatusServiceUnavailable, `{"error":{"code":503,"message":"Busy"}}`, nil},
		{"transport failure", http.StatusBadGateway, "connection reset by peer", errors.New("connection reset by peer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				upstream := &antigravityBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
					calls++
					time.Sleep(30 * time.Millisecond)
					if tc.err != nil {
						return nil, tc.err
					}
					return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				}}
				svc := &AntigravityGatewayService{}
				_, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
					ctx: context.Background(), account: &Account{ID: 57, Platform: PlatformAntigravity}, httpUpstream: upstream,
					action: "generateContent", body: []byte(`{}`), retryBudget: &antigravityRetryBudget{deadline: time.Now().Add(10 * time.Millisecond)},
				})
				failure := antigravityTerminalRetryError(err)
				require.NotNil(t, failure)
				require.Equal(t, tc.status, failure.StatusCode)
				require.Contains(t, string(failure.ResponseBody), tc.body)
				require.Equal(t, 1, calls)
			})
		})
	}
}

func TestAntigravityRetryBudgetForwardGeminiPreservesTerminalError(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon:generateContent", strings.NewReader(string(body)))
	original := []byte(`{"error":{"code":429,"message":"Quota exhausted"}}`)
	c.Set(antigravityRetryBudgetKey, &antigravityRetryBudget{deadline: time.Now().Add(-time.Second), status: 429, body: original, headers: http.Header{"X-Request-Id": {"original-429"}}})
	upstream := &recordingOKUpstream{}
	svc := &AntigravityGatewayService{
		settingService: NewSettingService(&antigravitySettingRepoStub{}, &config.Config{}),
		tokenProvider:  &AntigravityTokenProvider{}, httpUpstream: upstream,
	}
	account := &Account{ID: 56, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "test-token", "project_id": "test-project", "model_mapping": map[string]any{"gemini-4-argon": "gemini-4-argon"},
	}}
	_, err := svc.ForwardGemini(context.Background(), c, account, "gemini-4-argon", "generateContent", false, body, false)
	failure := antigravityTerminalRetryError(err)
	require.NotNil(t, failure)
	require.Equal(t, 429, failure.StatusCode)
	require.Equal(t, original, failure.ResponseBody)
	require.Equal(t, "original-429", failure.ResponseHeaders.Get("X-Request-Id"))
	deadline, ok := AntigravityRetryDeadline(c)
	require.True(t, ok)
	require.Equal(t, deadline, failure.RequestRetryDeadline)
	require.False(t, c.Writer.Written(), "the handler must receive the original terminal error, not a pre-written 502")
	require.Zero(t, upstream.calls)
}

func TestAntigravityRetryBudgetForwardGeminiCarriesDeadlineOnFailover(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon:generateContent", strings.NewReader(string(body)))
	original := `{"error":{"code":429,"message":"Resource has been exhausted"}}`
	calls := 0
	upstream := &antigravityBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(original))}, nil
	}}
	svc := &AntigravityGatewayService{
		settingService: NewSettingService(&antigravitySettingRepoStub{}, &config.Config{}),
		tokenProvider:  &AntigravityTokenProvider{}, httpUpstream: upstream, accountRepo: &stubAntigravityAccountRepo{},
	}
	account := &Account{ID: 58, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "test-token", "project_id": "test-project", "model_mapping": map[string]any{"gemini-4-argon": "gemini-4-argon"},
	}}
	_, err := svc.ForwardGemini(context.Background(), c, account, "gemini-4-argon", "generateContent", false, body, false)
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.True(t, failure.ShouldRetryNextAccount())
	require.Equal(t, 429, failure.StatusCode)
	require.Equal(t, original, string(failure.ResponseBody))
	deadline, ok := AntigravityRetryDeadline(c)
	require.True(t, ok)
	require.Equal(t, deadline, failure.RequestRetryDeadline, "queue and empty-pool retries must inherit this deadline")
	require.Equal(t, 1, calls, "generic 429 must not multiply same-account attempts")
	require.False(t, c.Writer.Written())
}

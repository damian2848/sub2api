//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 通用 429（无 RetryInfo、无模型信息）：重试循环会在 smartRetryActionContinue 之后
// 调一次 handleError 写模型冷却。Forward 层不得再对同一响应调用 handleUpstreamError。
const antigravityGeneric429Body = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Resource has been exhausted (e.g. check quota)."}}`

// errorHandledUpstream 每次调用都返回一个新的固定响应。
type errorHandledUpstream struct {
	status int
	body   string
	calls  int
}

func (u *errorHandledUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	return &http.Response{
		StatusCode: u.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(u.body)),
	}, nil
}

func (u *errorHandledUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func newErrorHandledAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Name:        "acc-error-handled",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "token",
			"expires_at":   time.Now().Add(time.Hour).Format(time.RFC3339),
			"project_id":   "proj",
		},
	}
}

// modelRateLimitWriteAttempts 返回冷却写入器被调用的次数（含被去重跳过的）。
// 只看仓储层写入次数无法发现 errorHandled 透传失效：第二次写会被去重器吞掉。
func modelRateLimitWriteAttempts(svc *AntigravityGatewayService) int {
	svc.modelRateLimitWriter.mu.Lock()
	defer svc.modelRateLimitWriter.mu.Unlock()
	return svc.modelRateLimitWriter.attempts
}

func TestAntigravityGeneric429_CooldownWrittenOncePerForwardPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("ForwardGemini", func(t *testing.T) {
		upstream := &errorHandledUpstream{status: http.StatusTooManyRequests, body: antigravityGeneric429Body}
		repo := &stubAntigravityAccountRepo{}
		svc := &AntigravityGatewayService{
			tokenProvider: &AntigravityTokenProvider{},
			httpUpstream:  upstream,
			accountRepo:   repo,
		}
		body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", bytes.NewReader(body))

		_, err := svc.ForwardGemini(context.Background(), c, newErrorHandledAccount(901), "gemini-2.5-flash", "generateContent", false, body, false)

		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
		require.Equal(t, 1, upstream.calls)
		require.Len(t, repo.modelRateLimitCalls, 1, "generic 429 must write the model cooldown exactly once")
		require.Equal(t, 1, modelRateLimitWriteAttempts(svc), "the cooldown write must be requested once, not requested twice and deduplicated")
		require.Empty(t, repo.rateCalls)
	})

	t.Run("Forward", func(t *testing.T) {
		upstream := &errorHandledUpstream{status: http.StatusTooManyRequests, body: antigravityGeneric429Body}
		repo := &stubAntigravityAccountRepo{}
		svc := &AntigravityGatewayService{
			tokenProvider: &AntigravityTokenProvider{},
			httpUpstream:  upstream,
			accountRepo:   repo,
		}
		body, err := json.Marshal(map[string]any{
			"model":      "claude-sonnet-4-5",
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
			"max_tokens": 1,
			"stream":     false,
		})
		require.NoError(t, err)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

		_, err = svc.Forward(context.Background(), c, newErrorHandledAccount(902), body, false)

		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
		require.Equal(t, 1, upstream.calls)
		require.Len(t, repo.modelRateLimitCalls, 1, "generic 429 must write the model cooldown exactly once")
		require.Equal(t, 1, modelRateLimitWriteAttempts(svc), "the cooldown write must be requested once, not requested twice and deduplicated")
		require.Empty(t, repo.rateCalls)
	})

	t.Run("ForwardAsChatCompletions", func(t *testing.T) {
		upstream := &errorHandledUpstream{status: http.StatusTooManyRequests, body: antigravityGeneric429Body}
		repo := &stubAntigravityAccountRepo{}
		svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
		svc.accountRepo = repo
		account := newAntigravityCompatAccount(AccountTypeOAuth)
		body := []byte(`{"model":"claude-sonnet-4-5","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
		c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)

		_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)

		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
		require.Equal(t, 1, upstream.calls)
		require.Len(t, repo.modelRateLimitCalls, 1, "generic 429 must write the model cooldown exactly once")
		require.Equal(t, 1, modelRateLimitWriteAttempts(svc), "the cooldown write must be requested once, not requested twice and deduplicated")
		require.Empty(t, repo.rateCalls)
	})
}

// ErrorPolicyMatched：applyErrorPolicy 已经调过 handleError，循环结果必须带 errorHandled，
// Forward 层据此跳过第二次 handleUpstreamError（跳过逻辑由上面的通用 429 用例覆盖）。
// 自定义错误码只对 API Key 账号生效，而 Antigravity API Key 账号拿不到 Forward 的 token，
// 所以这里直接驱动重试循环。
func TestAntigravityErrorPolicyMatched_LoopMarksErrorHandled(t *testing.T) {
	upstream := &errorHandledUpstream{status: 529, body: `{"error":{"code":529,"message":"overloaded"}}`}
	repo := &errorPolicyRepoStub{}
	svc := &AntigravityGatewayService{
		accountRepo:      repo,
		rateLimitService: NewRateLimitService(repo, nil, &config.Config{}, nil, nil),
	}
	account := newErrorHandledAccount(903)
	account.Type = AccountTypeAPIKey
	account.Credentials["custom_error_codes_enabled"] = true
	account.Credentials["custom_error_codes"] = []any{float64(529)}
	handleErrorCalls := 0

	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		ctx:            context.Background(),
		prefix:         "[test]",
		account:        account,
		accessToken:    "token",
		action:         "streamGenerateContent",
		body:           []byte(`{"request":{}}`),
		httpUpstream:   upstream,
		accountRepo:    repo,
		requestedModel: "gemini-2.5-flash",
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			handleErrorCalls++
			return svc.handleUpstreamError(ctx, prefix, account, statusCode, headers, body, requestedModel, groupID, sessionHash, isStickySession)
		},
	})

	require.NoError(t, err)
	require.Equal(t, 529, result.resp.StatusCode)
	require.Equal(t, 1, upstream.calls, "matched error policy must not retry")
	require.Equal(t, 1, handleErrorCalls)
	require.Equal(t, 1, repo.setErrCalls)
	require.True(t, result.errorHandled, "matched error policy already handled the error")
}

// 循环未处理的普通 4xx：errorHandled 为 false，Forward 层仍调用一次 handleUpstreamError。
func TestAntigravityPlain4xx_StillHandledByForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const orgDisabled = `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"This organization has been disabled."}}`

	t.Run("loop leaves flag unset", func(t *testing.T) {
		upstream := &errorHandledUpstream{status: http.StatusBadRequest, body: orgDisabled}
		result, err := (&AntigravityGatewayService{}).antigravityRetryLoop(antigravityRetryLoopParams{
			ctx:            context.Background(),
			prefix:         "[test]",
			account:        newErrorHandledAccount(904),
			accessToken:    "token",
			action:         "streamGenerateContent",
			body:           []byte(`{"request":{}}`),
			httpUpstream:   upstream,
			requestedModel: "gemini-2.5-flash",
			handleError: func(context.Context, string, *Account, int, http.Header, []byte, string, int64, string, bool) *handleModelRateLimitResult {
				t.Fatal("plain 4xx must not be handled inside the loop")
				return nil
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, result.resp.StatusCode)
		require.False(t, result.errorHandled)
	})

	t.Run("ForwardGemini handles once", func(t *testing.T) {
		upstream := &errorHandledUpstream{status: http.StatusBadRequest, body: orgDisabled}
		repo := &errorPolicyRepoStub{}
		svc := &AntigravityGatewayService{
			tokenProvider:    &AntigravityTokenProvider{},
			httpUpstream:     upstream,
			accountRepo:      repo,
			rateLimitService: NewRateLimitService(repo, nil, &config.Config{}, nil, nil),
		}
		body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", bytes.NewReader(body))

		_, _ = svc.ForwardGemini(context.Background(), c, newErrorHandledAccount(906), "gemini-2.5-flash", "generateContent", false, body, false)

		require.Equal(t, 1, upstream.calls)
		require.Equal(t, 1, repo.setErrCalls, "Forward must still handle errors the loop left unhandled")
	})
}

// 容量重试中遇到 429：handleSmartRetry 已调 handleError，应透传 errorHandled。
func TestHandleSmartRetry_CapacityRetryHits429_MarksErrorHandled(t *testing.T) {
	const model = "gemini-error-handled-capacity"
	modelCapacityExhaustedMu.Lock()
	delete(modelCapacityExhaustedUntil, model)
	modelCapacityExhaustedMu.Unlock()

	respBody := []byte(`{"error":{"code":503,"status":"UNAVAILABLE","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"model":"` + model + `"},"reason":"MODEL_CAPACITY_EXHAUSTED"}]}}`)
	resp := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(respBody))}
	upstream := &mockSmartRetryUpstream{
		responses: []*http.Response{{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(antigravityGeneric429Body))}},
		errors:    []error{nil},
	}
	handleErrorCalls := 0
	params := antigravityRetryLoopParams{
		ctx:          context.Background(),
		prefix:       "[test]",
		account:      &Account{ID: 905, Type: AccountTypeOAuth, Platform: PlatformAntigravity},
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		accountRepo:  &stubAntigravityAccountRepo{},
		httpUpstream: upstream,
		handleError: func(context.Context, string, *Account, int, http.Header, []byte, string, int64, string, bool) *handleModelRateLimitResult {
			handleErrorCalls++
			return nil
		},
	}

	result := (&AntigravityGatewayService{}).handleSmartRetry(params, resp, respBody, "https://ag-1.test", 0, []string{"https://ag-1.test"})

	require.Equal(t, smartRetryActionBreakWithResp, result.action)
	require.NotNil(t, result.resp)
	require.Equal(t, http.StatusTooManyRequests, result.resp.StatusCode)
	require.Equal(t, 1, handleErrorCalls)
	require.True(t, result.errorHandled)
}

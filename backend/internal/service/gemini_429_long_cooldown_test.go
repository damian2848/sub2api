//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	// 无 RetryInfo 的 AI Studio 429：冷却到太平洋时间午夜（长冷却）。
	gemini429LongCooldownBody = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`
	// 带 2s RetryInfo 的 429：短冷却，同号退避重试可能成功。
	gemini429ShortRetryInfoBody = `{"error":{"code":429,"message":"Resource has been exhausted.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2s"}]}}`
)

type gemini429CountingRepo struct {
	geminiSingleCheckRepoStub
	rateLimitedCalls []time.Time
}

func (r *gemini429CountingRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	r.rateLimitedCalls = append(r.rateLimitedCalls, resetAt)
	return nil
}

func newGemini429Service(t *testing.T, skipLongCooldown bool, retries int, responses ...geminiSequenceUpstreamResponse) (*GeminiMessagesCompatService, *geminiSequenceUpstreamStub, *gemini429CountingRepo) {
	t.Helper()
	svc, upstream, _ := newGeminiSingleCheckService(responses...)
	repo := &gemini429CountingRepo{}
	svc.accountRepo = repo
	svc.rateLimitService = NewRateLimitService(repo, nil, svc.cfg, nil, nil)
	svc.cfg.Gateway.MaxSameAccountRetriesGemini = retries
	svc.cfg.Gateway.Scheduling.GeminiLongCooldownSkipSameAccountRetry = skipLongCooldown
	// 退避等待清零，测试只关心次数。
	old := geminiBackoffDelay
	geminiBackoffDelay = func(int) time.Duration { return 0 }
	t.Cleanup(func() { geminiBackoffDelay = old })
	return svc, upstream, repo
}

func TestGemini429LongCooldown_FailsOverWithoutSameAccountRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, fw := range geminiSingleCheckForwarders {
		t.Run(fw.name, func(t *testing.T) {
			svc, upstream, repo := newGemini429Service(t, true, 4,
				geminiSequenceUpstreamResponse{status: http.StatusTooManyRequests, body: gemini429LongCooldownBody})

			err, _ := fw.forward(t, svc, geminiSameAccountRetryTestAccount())

			var failoverErr *UpstreamFailoverError
			require.True(t, errors.As(err, &failoverErr), "长冷却 429 应直接换号，got %T: %v", err, err)
			require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
			require.Equal(t, 1, upstream.calls, "冷却长于同号退避上限时不应同号重试")
			require.Len(t, repo.rateLimitedCalls, 1, "账号冷却只写一次")
			require.Greater(t, time.Until(repo.rateLimitedCalls[0]), geminiRetryMaxDelay)
		})
	}
}

func TestGemini429LongCooldown_SwitchOffKeepsRetriesButWritesOncePerResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, fw := range geminiSingleCheckForwarders {
		t.Run(fw.name, func(t *testing.T) {
			svc, upstream, repo := newGemini429Service(t, false, 1,
				geminiSequenceUpstreamResponse{status: http.StatusTooManyRequests, body: gemini429LongCooldownBody})

			err, _ := fw.forward(t, svc, geminiSameAccountRetryTestAccount())

			var failoverErr *UpstreamFailoverError
			require.True(t, errors.As(err, &failoverErr), "got %T: %v", err, err)
			require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
			require.Equal(t, 2, upstream.calls, "开关关闭时保持原有同号重试次数")
			// 每个上游 429 响应写一次；循环后不再对最后一个响应重复写（旧行为是 3 次）。
			require.Len(t, repo.rateLimitedCalls, 2)
		})
	}
}

func TestGemini429ShortRetryInfo_StillRetriesSameAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, fw := range geminiSingleCheckForwarders {
		t.Run(fw.name, func(t *testing.T) {
			svc, upstream, repo := newGemini429Service(t, true, 1,
				geminiSequenceUpstreamResponse{status: http.StatusTooManyRequests, body: gemini429ShortRetryInfoBody},
				geminiSequenceUpstreamResponse{status: http.StatusOK, body: geminiRetryTestOKBody})

			err, _ := fw.forward(t, svc, geminiSameAccountRetryTestAccount())

			require.NoError(t, err)
			require.Equal(t, 2, upstream.calls, "短 RetryInfo 冷却仍应同号重试")
			require.Len(t, repo.rateLimitedCalls, 1)
			require.LessOrEqual(t, time.Until(repo.rateLimitedCalls[0]), geminiRetryMaxDelay)
		})
	}
}

func TestSleepGeminiBackoff_ReturnsOnContextCancel(t *testing.T) {
	old := geminiBackoffDelay
	geminiBackoffDelay = func(int) time.Duration { return 10 * time.Second }
	t.Cleanup(func() { geminiBackoffDelay = old })

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	err := sleepGeminiBackoff(ctx, 1)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), time.Second)

	require.ErrorIs(t, sleepGeminiBackoff(ctx, 1), context.Canceled, "已取消的 ctx 立即返回")

	geminiBackoffDelay = func(int) time.Duration { return time.Millisecond }
	require.NoError(t, sleepGeminiBackoff(context.Background(), 1), "未取消时等满退避后返回 nil")
}

func TestGeminiForward_ClientCancelDuringBackoffReturnsImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, upstream, _ := newGemini429Service(t, true, 4,
		geminiSequenceUpstreamResponse{status: http.StatusServiceUnavailable, body: geminiRetryTest503Body})
	geminiBackoffDelay = func(int) time.Duration { return 10 * time.Second }

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := []byte(`{"model":"gemini-2.5-flash","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	start := time.Now()
	_, err := svc.Forward(ctx, c, geminiSameAccountRetryTestAccount(), body)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), time.Second, "退避等待应随 ctx 取消立即结束")
	require.Equal(t, 1, upstream.calls)
}

//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const antigravityModelRateLimitBody = `{
	"error": {
		"status": "RESOURCE_EXHAUSTED",
		"details": [
			{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "metadata": {"model": "gemini-3-flash"}, "reason": "RATE_LIMIT_EXCEEDED"},
			{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "30s"}
		]
	}
}`

// ctxRecordingAntigravityRepo 记录写入时 ctx 是否已取消，并可模拟慢写/失败。
type ctxRecordingAntigravityRepo struct {
	AccountRepository
	mu             sync.Mutex
	modelCalls     int
	rateCalls      int
	ctxErrs        []error
	writeDelay     time.Duration
	failModelWrite atomic.Bool
}

func (r *ctxRecordingAntigravityRepo) SetModelRateLimit(ctx context.Context, _ int64, _ string, _ time.Time, _ ...string) error {
	if r.writeDelay > 0 {
		time.Sleep(r.writeDelay)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modelCalls++
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	if r.failModelWrite.Load() {
		return errors.New("db down")
	}
	return nil
}

func (r *ctxRecordingAntigravityRepo) SetRateLimited(ctx context.Context, _ int64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rateCalls++
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	return nil
}

func (r *ctxRecordingAntigravityRepo) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.modelCalls
}

type ctxRecordingSessionCache struct {
	GatewayCache
	ctxErrs []error
}

func (c *ctxRecordingSessionCache) DeleteSessionAccountID(ctx context.Context, _ int64, _ string) error {
	c.ctxErrs = append(c.ctxErrs, ctx.Err())
	return nil
}

func newStateWriteTestAccount(id int64) *Account {
	return &Account{ID: id, Name: "acc-state-write", Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
}

func TestAntigravityHandleUpstreamError_CancelledRequestCtxStillPersistsCooldown(t *testing.T) {
	repo := &ctxRecordingAntigravityRepo{}
	cache := &ctxRecordingSessionCache{}
	svc := &AntigravityGatewayService{accountRepo: repo, cache: cache}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := svc.handleUpstreamError(ctx, "[test]", newStateWriteTestAccount(701), http.StatusTooManyRequests, http.Header{},
		[]byte(antigravityModelRateLimitBody), "gemini-3-flash", 9, "sticky", false)

	require.NotNil(t, result)
	require.NotNil(t, result.SwitchError)
	require.Equal(t, 1, repo.modelCalls, "model cooldown must be written even after the client hung up")
	require.Equal(t, []error{nil}, repo.ctxErrs, "state write must not see the cancelled request ctx")
	require.Equal(t, []error{nil}, cache.ctxErrs, "sticky session cleanup must not see the cancelled request ctx")
}

func TestAntigravityHandleUpstreamError_CancelledRequestCtxStillPersistsAccountFallback(t *testing.T) {
	repo := &ctxRecordingAntigravityRepo{}
	svc := &AntigravityGatewayService{accountRepo: repo}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 无法解析模型 key 时兜底为账号级限流（SetRateLimited）。
	svc.handleUpstreamError(ctx, "[test]", newStateWriteTestAccount(702), http.StatusTooManyRequests, http.Header{},
		[]byte(`{"error":{"code":429,"message":"Resource has been exhausted"}}`), "", 0, "", false)

	require.Equal(t, 1, repo.rateCalls)
	require.Equal(t, []error{nil}, repo.ctxErrs)
}

func TestAntigravityModelRateLimit_ConcurrentWritesCoalesce(t *testing.T) {
	repo := &ctxRecordingAntigravityRepo{writeDelay: 30 * time.Millisecond}
	svc := &AntigravityGatewayService{accountRepo: repo}
	resetAt := time.Now().Add(time.Minute)

	var wg sync.WaitGroup
	var okCount atomic.Int32
	start := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// 每个请求持有自己的账号快照，与线上一致。
			if svc.setAntigravityModelRateLimits(context.Background(), repo, newStateWriteTestAccount(703), "gemini-3-flash", "[test]", 429, resetAt, false) {
				okCount.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, 1, repo.calls(), "concurrent 429s on the same account x model must write once")
	require.Equal(t, int32(50), okCount.Load(), "every caller sees the cooldown as applied")
}

func TestAntigravityModelRateLimit_DedupWindowIsShortAndRespectsLongerReset(t *testing.T) {
	repo := &ctxRecordingAntigravityRepo{}
	svc := &AntigravityGatewayService{accountRepo: repo}
	set := func(resetAt time.Time) bool {
		account := newStateWriteTestAccount(704)
		ok := svc.setAntigravityModelRateLimits(context.Background(), repo, account, "gemini-3-flash", "[test]", 429, resetAt, false)
		if ok {
			got := account.modelRateLimitResetAt("gemini-3-flash")
			require.NotNil(t, got, "request-local snapshot is updated even when the DB write is skipped")
		}
		return ok
	}
	base := time.Now().Add(time.Minute)

	require.True(t, set(base))
	require.Equal(t, 1, repo.calls())

	// 同一种 429 重算出的冷却晚几百毫秒：窗口内视为重复。
	require.True(t, set(base.Add(500*time.Millisecond)))
	require.Equal(t, 1, repo.calls())

	// 更长的冷却（例如配额耗尽）必须写入。
	require.True(t, set(base.Add(time.Hour)))
	require.Equal(t, 2, repo.calls())

	// 窗口过期后（例如管理员清除了冷却），新 429 照常写入。
	key := antigravityModelRateLimitWriteKey(704, "gemini-3-flash")
	svc.modelRateLimitWriter.mu.Lock()
	rec := svc.modelRateLimitWriter.recent[key]
	rec.writtenAt = time.Now().Add(-antigravityModelRateLimitDedupWindow - time.Second)
	svc.modelRateLimitWriter.recent[key] = rec
	svc.modelRateLimitWriter.mu.Unlock()
	require.True(t, set(base.Add(time.Hour)))
	require.Equal(t, 3, repo.calls())
}

func TestAntigravityModelRateLimit_FailedWriteIsNotDeduplicated(t *testing.T) {
	repo := &ctxRecordingAntigravityRepo{}
	repo.failModelWrite.Store(true)
	svc := &AntigravityGatewayService{accountRepo: repo}
	resetAt := time.Now().Add(time.Minute)

	require.False(t, svc.setAntigravityModelRateLimits(context.Background(), repo, newStateWriteTestAccount(705), "gemini-3-flash", "[test]", 429, resetAt, false))
	repo.failModelWrite.Store(false)
	require.True(t, svc.setAntigravityModelRateLimits(context.Background(), repo, newStateWriteTestAccount(705), "gemini-3-flash", "[test]", 429, resetAt, false))
	require.Equal(t, 2, repo.calls(), "a failed write must not suppress the retry")
}

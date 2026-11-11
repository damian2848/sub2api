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

// 出窗口的旧记录不能被沿用：DB 里的长冷却可能已被管理员/测试恢复清掉，
// 之后一次短冷却写入只能记录它自己的 reset，否则会吞掉随后更长的冷却。
func TestAntigravityModelRateLimit_RecordsOnlyResetActuallyWritten(t *testing.T) {
	var w antigravityModelRateLimitWriter
	writes := 0
	doWrite := func() bool { writes++; return true }
	key := antigravityModelRateLimitWriteKey(706, "gemini-3-flash")
	now := time.Now()

	ok, skipped := w.write(706, "gemini-3-flash", now.Add(5*time.Hour), doWrite)
	require.True(t, ok)
	require.False(t, skipped)

	// 模拟窗口过期（期间冷却被清除），再来一次短冷却。
	w.mu.Lock()
	rec := w.recent[key]
	rec.writtenAt = time.Now().Add(-antigravityModelRateLimitDedupWindow - time.Second)
	w.recent[key] = rec
	w.mu.Unlock()
	short := time.Now().Add(time.Minute)
	ok, skipped = w.write(706, "gemini-3-flash", short, doWrite)
	require.True(t, ok)
	require.False(t, skipped)
	w.mu.Lock()
	require.True(t, w.recent[key].resetAt.Equal(short), "record must hold the reset just written, not the stale longer one")
	w.mu.Unlock()

	// 窗口内更长的配额冷却必须照写。
	ok, skipped = w.write(706, "gemini-3-flash", now.Add(4*time.Hour), doWrite)
	require.True(t, ok)
	require.False(t, skipped)
	require.Equal(t, 3, writes)
}

// doWrite panic 时 inflight 必须被清理、done 必须关闭：后续写入不能永久阻塞，且会重新自己写。
func TestAntigravityModelRateLimit_PanickingWriteReleasesFlight(t *testing.T) {
	var w antigravityModelRateLimitWriter
	resetAt := time.Now().Add(time.Minute)
	key := antigravityModelRateLimitWriteKey(707, "gemini-3-flash")

	started := make(chan struct{})
	release := make(chan struct{})
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		w.write(707, "gemini-3-flash", resetAt, func() bool {
			close(started)
			<-release
			panic("boom")
		})
	}()
	<-started

	// 一个等待者在 panic 之前已挂在同一 flight 上。
	waiterDone := make(chan [2]bool, 1)
	waiterWrites := 0
	go func() {
		ok, skipped := w.write(707, "gemini-3-flash", resetAt, func() bool { waiterWrites++; return true })
		waiterDone <- [2]bool{ok, skipped}
	}()
	// 给等待者时间进入 write 并挂到 flight 上（即使没来得及，也走同样的断言）。
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case p := <-panicked:
		require.Equal(t, "boom", p, "panic must still propagate to the caller")
	case <-time.After(2 * time.Second):
		t.Fatal("panicking writer did not return")
	}
	select {
	case res := <-waiterDone:
		require.True(t, res[0])
		require.False(t, res[1], "waiter must write itself after the in-flight write panicked")
		require.Equal(t, 1, waiterWrites)
	case <-time.After(2 * time.Second):
		t.Fatal("waiter blocked forever on a panicked flight")
	}

	w.mu.Lock()
	_, stillInflight := w.inflight[key]
	w.mu.Unlock()
	require.False(t, stillInflight)

	// 之后的新写入也不阻塞（被等待者的成功写入去重）。
	done := make(chan struct{})
	go func() {
		w.write(707, "gemini-3-flash", resetAt, func() bool { return true })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subsequent write blocked")
	}
}

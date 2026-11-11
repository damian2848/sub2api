package service

import (
	"context"
	"strconv"
	"sync"
	"time"
)

const (
	// antigravityStateWriteTimeout 限制账号状态写入（模型冷却、账号限流、粘性会话清理）的耗时。
	antigravityStateWriteTimeout = 3 * time.Second
	// antigravityModelRateLimitDedupWindow 内同一账号×模型的冷却已写过且不短于本次时，跳过重复写库。
	// 窗口刻意很短：管理员手动清除冷却后，下一次 429 最多被吞掉这几秒。
	antigravityModelRateLimitDedupWindow = 5 * time.Second
	// antigravityModelRateLimitDedupTolerance 允许新冷却比已写入的晚这么多仍视为重复
	// （同一种 429 的默认冷却每次重算都会晚几毫秒）。
	antigravityModelRateLimitDedupTolerance = time.Second
	// antigravityModelRateLimitDedupMaxEntries 超过后写入时顺带清掉过期条目。
	antigravityModelRateLimitDedupMaxEntries = 4096
)

// antigravityStateCtx 返回用于账号状态写入的 ctx：保留请求 ctx 的值，但不随客户端断开取消，
// 并带独立超时。客户端在拿到 429 后断开不能让账号继续处于可调度状态。
func antigravityStateCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), antigravityStateWriteTimeout)
}

type antigravityModelRateLimitWrite struct {
	resetAt   time.Time
	writtenAt time.Time
}

type antigravityModelRateLimitFlight struct {
	done    chan struct{}
	resetAt time.Time
	ok      bool
}

// antigravityModelRateLimitWriter 合并同一账号×模型的并发冷却写入，并在短窗口内去重。
// SQL 端本来就保留较长的 reset 值，这里只省掉不会改变结果的 UPDATE / outbox / 快照同步。
type antigravityModelRateLimitWriter struct {
	mu       sync.Mutex
	recent   map[string]antigravityModelRateLimitWrite
	inflight map[string]*antigravityModelRateLimitFlight
}

func antigravityModelRateLimitWriteKey(accountID int64, modelKey string) string {
	return strconv.FormatInt(accountID, 10) + "|" + modelKey
}

func (w *antigravityModelRateLimitWriter) covers(rec antigravityModelRateLimitWrite, resetAt, now time.Time) bool {
	return now.Sub(rec.writtenAt) < antigravityModelRateLimitDedupWindow &&
		rec.resetAt.After(now) &&
		!rec.resetAt.Before(resetAt.Add(-antigravityModelRateLimitDedupTolerance))
}

// write 执行或复用一次冷却写入。返回 (ok, skipped)：skipped 表示本次没有自己写库，
// 而是复用了窗口内或同时在途的、不短于本次的写入结果。
func (w *antigravityModelRateLimitWriter) write(accountID int64, modelKey string, resetAt time.Time, doWrite func() bool) (ok bool, skipped bool) {
	key := antigravityModelRateLimitWriteKey(accountID, modelKey)
	for {
		now := time.Now()
		w.mu.Lock()
		if rec, exists := w.recent[key]; exists && w.covers(rec, resetAt, now) {
			w.mu.Unlock()
			return true, true
		}
		if flight, exists := w.inflight[key]; exists {
			w.mu.Unlock()
			<-flight.done
			if flight.ok && !flight.resetAt.Before(resetAt.Add(-antigravityModelRateLimitDedupTolerance)) {
				return true, true
			}
			// 在途写入失败或比本次短：重新判断，必要时自己写。
			continue
		}
		if w.inflight == nil {
			w.inflight = make(map[string]*antigravityModelRateLimitFlight)
		}
		flight := &antigravityModelRateLimitFlight{done: make(chan struct{}), resetAt: resetAt}
		w.inflight[key] = flight
		w.mu.Unlock()

		flight.ok = doWrite()

		w.mu.Lock()
		delete(w.inflight, key)
		if flight.ok {
			if w.recent == nil {
				w.recent = make(map[string]antigravityModelRateLimitWrite)
			}
			if len(w.recent) >= antigravityModelRateLimitDedupMaxEntries {
				for k, rec := range w.recent {
					if now.Sub(rec.writtenAt) >= antigravityModelRateLimitDedupWindow || !rec.resetAt.After(now) {
						delete(w.recent, k)
					}
				}
			}
			rec := antigravityModelRateLimitWrite{resetAt: resetAt, writtenAt: time.Now()}
			if prev, exists := w.recent[key]; exists && prev.resetAt.After(rec.resetAt) {
				rec.resetAt = prev.resetAt
			}
			w.recent[key] = rec
		}
		w.mu.Unlock()
		close(flight.done)
		return flight.ok, false
	}
}

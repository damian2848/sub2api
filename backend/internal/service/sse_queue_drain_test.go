//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 回归：首输出超时分支 drain 队列时必须归还 queueBudget。
// 场景：队列里有一条已 acquire 未出队的大事件 e1，drain 接收它；生产者随后从
// scanner 缓冲吐出下一条 e2，w1+w2 超过预算。若 drain 不 release，生产者的
// acquire 永久阻塞（done 要等 handler 返回才关闭），events 不关闭，drain 永不结束。
func TestDrainSSEEventQueueReleasesBudgetSoProducerCanFinish(t *testing.T) {
	type ev struct{ line string }
	budget := newSSEQueueBudget(&config.Config{Gateway: config.GatewayConfig{SSEQueueMaxBytes: 1000}})
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	events := make(chan ev, 1)
	e1 := ev{line: string(make([]byte, 900))}
	require.True(t, budget.acquire(len(e1.line), done))
	events <- e1

	producerStarted := make(chan struct{})
	go func() {
		defer close(events)
		close(producerStarted)
		e2 := ev{line: string(make([]byte, 500))}
		if !budget.acquire(len(e2.line), done) {
			return
		}
		select {
		case events <- e2:
		case <-done:
			budget.release(len(e2.line))
		}
	}()
	<-producerStarted

	var dequeued []int
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		drainSSEEventQueue(events, budget,
			func(e ev) int { return len(e.line) },
			func(e ev) { dequeued = append(dequeued, len(e.line)) })
	}()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("drain deadlocked: producer blocked on unreleased queue budget")
	}
	require.Equal(t, []int{900, 500}, dequeued)
	require.Zero(t, budget.used.Load(), "every dequeued event must return its budget")
}

// release(0) 是空操作：读错误事件（line 为空）经过 drain 不会把预算记成负数。
func TestDrainSSEEventQueueZeroSizeEvents(t *testing.T) {
	type ev struct {
		line string
		err  error
	}
	budget := newSSEQueueBudget(nil)
	events := make(chan ev, 2)
	events <- ev{err: errSSETransportLimit}
	events <- ev{}
	close(events)

	calls := 0
	drainSSEEventQueue(events, budget, func(e ev) int { return len(e.line) }, func(ev) { calls++ })
	require.Equal(t, 2, calls)
	require.Zero(t, budget.used.Load())
}

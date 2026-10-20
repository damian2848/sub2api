package handler

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFailoverRequestBudgetSurvivesAccountChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		fs := NewFailoverState(10, false)
		deadline := time.Now().Add(2500 * time.Millisecond)
		first := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, RequestRetryDeadline: deadline}
		require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, nil, 1, service.PlatformAntigravity, 0, first))
		second := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, RequestRetryDeadline: time.Now().Add(time.Hour)}
		require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, nil, 2, service.PlatformAntigravity, 0, second))
		require.Equal(t, deadline, fs.requestRetryDeadline, "changing accounts cannot extend the budget")
		time.Sleep(2 * time.Second)
		last := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, ResponseBody: []byte("last quota error")}
		require.Equal(t, FailoverExhausted, fs.HandleFailoverError(ctx, nil, 3, service.PlatformAntigravity, 0, last))
		require.Same(t, last, fs.LastFailoverErr)
		require.Equal(t, 2, fs.SwitchCount)
		require.NoError(t, ctx.Err(), "budget exhaustion is not client cancellation")
	})
}

func TestFailoverRequestBudgetEndsEmptyPoolBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		fs := NewFailoverState(3, false)
		start := time.Now()
		fs.LastFailoverErr = &service.UpstreamFailoverError{
			StatusCode: http.StatusServiceUnavailable, RequestRetryDeadline: start.Add(5 * time.Second),
		}
		require.Equal(t, FailoverContinue, fs.HandleSelectionExhausted(ctx))
		require.Equal(t, FailoverContinue, fs.HandleSelectionExhausted(ctx))
		fs.FailedAccountIDs[17] = struct{}{}
		require.Equal(t, FailoverExhausted, fs.HandleSelectionExhausted(ctx))
		require.Equal(t, 4*time.Second, time.Since(start), "do not start a backoff that cannot fit")
		require.Contains(t, fs.FailedAccountIDs, int64(17), "exhaustion must not reopen failed accounts")
		require.Equal(t, http.StatusServiceUnavailable, fs.LastFailoverErr.StatusCode)
		require.NoError(t, ctx.Err())
	})
}

func TestFailoverRequestBudgetBoundsSameAccountDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		fs := NewFailoverState(3, false)
		start := time.Now()
		failure := &service.UpstreamFailoverError{
			StatusCode: http.StatusTooManyRequests, RetryableOnSameAccount: true,
			RequestRetryDeadline: start.Add(100 * time.Millisecond),
		}
		unscheduler := &mockTempUnscheduler{}
		require.Equal(t, FailoverExhausted, fs.HandleFailoverError(ctx, unscheduler, 1, service.PlatformAntigravity, 3, failure))
		require.Equal(t, time.Duration(0), time.Since(start))
		require.Empty(t, unscheduler.calls, "local time budget must not penalize account health")
		require.NoError(t, ctx.Err())
	})
}

func TestFailoverRequestBudgetPreservesClientCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(100*time.Millisecond, cancel)
		failure := &service.UpstreamFailoverError{
			StatusCode: http.StatusTooManyRequests, RetryableOnSameAccount: true,
			RequestRetryDeadline: time.Now().Add(10 * time.Second),
		}
		fs := NewFailoverState(3, false)
		require.Equal(t, FailoverCanceled, fs.HandleFailoverError(ctx, &mockTempUnscheduler{}, 1, service.PlatformAntigravity, 3, failure))
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})
}

func TestFailoverRequestBudgetTerminalErrorCannotReopenPool(t *testing.T) {
	fs := NewFailoverState(3, false)
	fs.LastFailoverErr = &service.UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable, NextAccountAction: service.NextAccountStop}
	require.Equal(t, FailoverExhausted, fs.HandleSelectionExhausted(context.Background()))
}

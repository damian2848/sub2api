package main

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeDrainServer struct {
	mu     sync.Mutex
	events []string
	onStop func()
}

func (s *fakeDrainServer) record(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *fakeDrainServer) Shutdown(ctx context.Context) error {
	s.record("shutdown")
	<-ctx.Done() // a long stream keeps graceful shutdown from finishing
	return ctx.Err()
}

func (s *fakeDrainServer) Close() error {
	s.record("close")
	if s.onStop != nil {
		s.onStop()
	}
	return nil
}

type fakeDrainLifecycle struct{ idle chan struct{} }

func (l *fakeDrainLifecycle) Wait(ctx context.Context) error {
	select {
	case <-l.idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestDrainServerWaitsForHandlersCancelledByClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lifecycle := &fakeDrainLifecycle{idle: make(chan struct{})}
		released := false
		srv := &fakeDrainServer{}
		srv.onStop = func() {
			// Close cancels request contexts; the handler then runs its
			// deferred slot release shortly afterwards.
			go func() {
				time.Sleep(200 * time.Millisecond)
				released = true
				close(lifecycle.idle)
			}()
		}
		start := time.Now()
		drainServer(srv, lifecycle, 5*time.Second, 3*time.Second)
		require.True(t, released, "shutdown must not return before the cancelled handler released its slot")
		require.Equal(t, 5*time.Second+200*time.Millisecond, time.Since(start))
		require.Equal(t, []string{"shutdown", "close"}, srv.events)
	})
}

func TestDrainServerPostCloseWaitIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lifecycle := &fakeDrainLifecycle{idle: make(chan struct{})}
		srv := &fakeDrainServer{}
		start := time.Now()
		drainServer(srv, lifecycle, 5*time.Second, 3*time.Second)
		require.Equal(t, 8*time.Second, time.Since(start), "a handler ignoring cancellation delays exit by at most the post-close budget")
	})
}

func TestDrainServerSkipsPostCloseWaitWhenDrained(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lifecycle := &fakeDrainLifecycle{idle: make(chan struct{})}
		close(lifecycle.idle)
		srv := &fakeDrainServer{}
		start := time.Now()
		drainServer(srv, lifecycle, 5*time.Second, 3*time.Second)
		require.Equal(t, 5*time.Second, time.Since(start))
		require.Equal(t, []string{"shutdown", "close"}, srv.events)
	})
}

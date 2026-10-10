package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func activityTestCache(t *testing.T) (*gatewayCache, *miniredis.Miniredis, func(time.Duration)) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	now := time.Now()
	mr.SetTime(now)
	advance := func(d time.Duration) {
		now = now.Add(d)
		mr.SetTime(now)
		mr.FastForward(d)
	}
	return &gatewayCache{rdb: client}, mr, advance
}

func TestSessionActivityIdleStartsAtCompletion(t *testing.T) {
	c, _, advance := activityTestCache(t)
	ctx := t.Context()
	idle, err := c.BeginSessionActivity(ctx, 1, "openai:chat", "first")
	require.NoError(t, err)
	require.False(t, idle, "unknown history protects existing bindings after an upgrade")
	// A 15-minute stream remains active through lease renewals.
	for range 30 {
		advance(30 * time.Second)
		require.NoError(t, c.RefreshSessionActivity(ctx, 1, "openai:chat", "first"))
	}
	idle, err = c.BeginSessionActivity(ctx, 1, "openai:chat", "overlap")
	require.NoError(t, err)
	require.False(t, idle)
	require.NoError(t, c.EndSessionActivity(ctx, 1, "openai:chat", "overlap"))
	require.NoError(t, c.EndSessionActivity(ctx, 1, "openai:chat", "first"))
	advance(service.SessionActivityIdleWindow - time.Millisecond)
	idle, err = c.BeginSessionActivity(ctx, 1, "openai:chat", "warm")
	require.NoError(t, err)
	require.False(t, idle, "the idle window starts after completion, not request start")
	require.NoError(t, c.EndSessionActivity(ctx, 1, "openai:chat", "warm"))
	advance(service.SessionActivityIdleWindow)
	idle, err = c.BeginSessionActivity(ctx, 1, "openai:chat", "idle")
	require.NoError(t, err)
	require.True(t, idle)
	allowed, err := c.SessionActivityAllowsMigration(ctx, 1, "openai:chat", "idle")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestSessionActivityConcurrentInstancesRevokeMigration(t *testing.T) {
	c, mr, advance := activityTestCache(t)
	otherClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = otherClient.Close() })
	other := &gatewayCache{rdb: otherClient}
	ctx := t.Context()
	_, err := c.BeginSessionActivity(ctx, 1, "chat", "history")
	require.NoError(t, err)
	require.NoError(t, c.EndSessionActivity(ctx, 1, "chat", "history"))
	advance(service.SessionActivityIdleWindow)

	var wg sync.WaitGroup
	idle := make([]bool, 2)
	errs := make([]error, 2)
	for i, cache := range []*gatewayCache{c, other} {
		wg.Go(func() {
			idle[i], errs[i] = cache.BeginSessionActivity(ctx, 1, "chat", []string{"a", "b"}[i])
		})
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.NotEqual(t, idle[0], idle[1], "only one request may observe an idle session")
	for _, token := range []string{"a", "b"} {
		allowed, err := c.SessionActivityAllowsMigration(ctx, 1, "chat", token)
		require.NoError(t, err)
		require.False(t, allowed, "the overlapping request revokes optional migration")
	}
	require.NoError(t, other.EndSessionActivity(ctx, 1, "chat", "b"))
	allowed, err := c.SessionActivityAllowsMigration(ctx, 1, "chat", "a")
	require.NoError(t, err)
	require.False(t, allowed, "finishing an overlap must not restore stale permission")
}

func TestSessionActivityExpiredLeaseAndIdempotentCompletion(t *testing.T) {
	c, mr, advance := activityTestCache(t)
	ctx := t.Context()
	_, err := c.BeginSessionActivity(ctx, 1, "chat", "crashed")
	require.NoError(t, err)
	advance(10 * time.Minute)
	idle, err := c.BeginSessionActivity(ctx, 1, "chat", "replacement")
	require.NoError(t, err)
	require.False(t, idle, "an expired worker starts a new idle window")
	require.NoError(t, c.EndSessionActivity(ctx, 1, "chat", "crashed"))
	keys := sessionActivityKeys(1, "chat")
	require.EqualValues(t, 1, c.rdb.ZCard(ctx, keys[1]).Val(), "late completion cannot release the replacement")
	require.NoError(t, c.EndSessionActivity(ctx, 1, "chat", "replacement"))
	advance(service.SessionActivityIdleWindow)
	require.NoError(t, c.EndSessionActivity(ctx, 1, "chat", "replacement"))
	idle, err = c.BeginSessionActivity(ctx, 1, "chat", "next")
	require.NoError(t, err)
	require.True(t, idle, "duplicate completion must not restart the idle clock")
	advance(service.SessionActivityRetention + time.Second)
	require.Empty(t, mr.Keys(), "abandoned sessions have bounded retention")
}

func TestSessionActivityIsolationAndErrors(t *testing.T) {
	c, _, advance := activityTestCache(t)
	ctx := t.Context()
	_, err := c.BeginSessionActivity(ctx, 1, "openai:chat", "history")
	require.NoError(t, err)
	require.NoError(t, c.EndSessionActivity(ctx, 1, "openai:chat", "history"))
	advance(service.SessionActivityIdleWindow)
	for _, scope := range []struct {
		group int64
		key   string
	}{{2, "openai:chat"}, {1, "gemini:chat"}, {1, "chat"}} {
		idle, err := c.BeginSessionActivity(ctx, scope.group, scope.key, "other")
		require.NoError(t, err)
		require.False(t, idle)
	}
	idle, err := c.BeginSessionActivity(ctx, 1, "openai:chat", "next")
	require.NoError(t, err)
	require.True(t, idle)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	allowed, err := c.SessionActivityAllowsMigration(cancelled, 1, "openai:chat", "next")
	require.Error(t, err)
	require.False(t, allowed)
}

func TestSessionActivityPartialEvictionKeepsBoundedRetention(t *testing.T) {
	c, mr, advance := activityTestCache(t)
	ctx := t.Context()
	_, err := c.BeginSessionActivity(ctx, 1, "chat", "crashed")
	require.NoError(t, err)
	keys := sessionActivityKeys(1, "chat")
	mr.Del(keys[0])
	advance(service.SessionActivityLease + time.Second)
	allowed, err := c.SessionActivityAllowsMigration(ctx, 1, "chat", "crashed")
	require.NoError(t, err)
	require.False(t, allowed)
	require.Equal(t, service.SessionActivityRetention, mr.TTL(keys[0]), "pruning must not recreate an immortal activity record")
}

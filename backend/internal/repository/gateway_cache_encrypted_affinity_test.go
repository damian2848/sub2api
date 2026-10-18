package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestGatewayCacheClaimSessionAccountIDAtomicOwnershipAndTTL(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &gatewayCache{rdb: client}
	ctx := context.Background()
	var wg sync.WaitGroup
	owners := make(chan int64, 2)
	errs := make(chan error, 2)
	for _, candidate := range []int64{10, 20} {
		wg.Add(1)
		go func(candidate int64) {
			defer wg.Done()
			owner, err := cache.ClaimSessionAccountID(ctx, 3, "encrypted-digest", candidate, time.Minute)
			owners <- owner
			errs <- err
		}(candidate)
	}
	wg.Wait()
	close(owners)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	first := <-owners
	require.Equal(t, first, <-owners, "concurrent claimants must observe one immutable owner")
	stored, err := cache.GetSessionAccountID(ctx, 3, "encrypted-digest")
	require.NoError(t, err)
	require.Equal(t, first, stored)
	server.FastForward(40 * time.Second)
	other := int64(10)
	if first == other {
		other = 20
	}
	owner, err := cache.ClaimSessionAccountID(ctx, 3, "encrypted-digest", other, time.Minute)
	require.NoError(t, err)
	require.Equal(t, first, owner)
	require.Equal(t, 20*time.Second, server.TTL(buildSessionKey(3, "encrypted-digest")), "conflicts cannot extend another owner's lifetime")
	owner, err = cache.ClaimSessionAccountID(ctx, 3, "encrypted-digest", first, time.Minute)
	require.NoError(t, err)
	require.Equal(t, first, owner)
	require.Equal(t, time.Minute, server.TTL(buildSessionKey(3, "encrypted-digest")))
	owner, err = cache.ClaimSessionAccountID(ctx, 4, "encrypted-digest", other, time.Minute)
	require.NoError(t, err)
	require.Equal(t, other, owner, "group isolation survives atomic claims")
}

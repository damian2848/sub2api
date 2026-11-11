//go:build unit

package service

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type replayedCommitBillingCache struct {
	BillingCache
	mu            sync.Mutex
	rateLimitKeys []int64
	balanceUsers  []int64
	subscriptions [][2]int64
}

func (c *replayedCommitBillingCache) InvalidateAPIKeyRateLimit(_ context.Context, keyID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rateLimitKeys = append(c.rateLimitKeys, keyID)
	return nil
}

func (c *replayedCommitBillingCache) InvalidateUserBalance(_ context.Context, userID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.balanceUsers = append(c.balanceUsers, userID)
	return nil
}

func (c *replayedCommitBillingCache) InvalidateSubscriptionCache(_ context.Context, userID, groupID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscriptions = append(c.subscriptions, [2]int64{userID, groupID})
	return nil
}

type replayedCommitAuthInvalidator struct {
	APIKeyQuotaUpdater
	keys []string
}

func (i *replayedCommitAuthInvalidator) InvalidateAuthCacheByKey(_ context.Context, key string) {
	i.keys = append(i.keys, key)
}

// When the billing transaction committed on an attempt whose result was lost,
// nothing synced the caches for that charge. They are dropped so the next
// check reloads the charged state from the DB.
func TestInvalidateBillingCachesAfterReplayedCommit(t *testing.T) {
	cache := &replayedCommitBillingCache{}
	auth := &replayedCommitAuthInvalidator{}
	deps := &billingDeps{billingCacheService: &BillingCacheService{cache: cache}}
	params := &postUsageBillingParams{
		User:               &User{ID: 11},
		APIKey:             &APIKey{ID: 22, Key: "sk-replayed"},
		Subscription:       &UserSubscription{ID: 33, GroupID: 44},
		IsSubscriptionBill: true,
		APIKeyService:      auth,
	}

	invalidateBillingCachesAfterReplayedCommit(context.Background(), params, deps)

	require.Equal(t, []int64{22}, cache.rateLimitKeys)
	require.Equal(t, []int64{11}, cache.balanceUsers)
	require.Equal(t, [][2]int64{{11, 44}}, cache.subscriptions)
	require.Equal(t, []string{"sk-replayed"}, auth.keys)

	// Balance billing has no subscription cache to drop; nil inputs are safe.
	cache2 := &replayedCommitBillingCache{}
	invalidateBillingCachesAfterReplayedCommit(context.Background(), &postUsageBillingParams{User: &User{ID: 5}, APIKey: &APIKey{ID: 6}}, &billingDeps{billingCacheService: &BillingCacheService{cache: cache2}})
	require.Empty(t, cache2.subscriptions)
	invalidateBillingCachesAfterReplayedCommit(context.Background(), nil, nil)
	invalidateBillingCachesAfterReplayedCommit(context.Background(), &postUsageBillingParams{}, &billingDeps{})
}

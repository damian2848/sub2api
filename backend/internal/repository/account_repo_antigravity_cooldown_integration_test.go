//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAntigravityModelCooldownRepository_KeepsLongestReset(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	cache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, cache)
	long := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	short := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	for _, platform := range []string{service.PlatformAntigravity, service.PlatformOpenAI} {
		t.Run(platform, func(t *testing.T) {
			account := mustCreateAccount(t, tx.Client(), &service.Account{Name: "cooldown-" + platform, Platform: platform})
			require.NoError(t, repo.SetModelRateLimit(ctx, account.ID, "gemini-3.8-flash-high", long, "quota_exhausted"))
			require.NoError(t, repo.SetModelRateLimit(ctx, account.ID, "gemini-3.8-flash-high", short, "throttle"))
			current, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			expected, reason := short, "throttle"
			if platform == service.PlatformAntigravity {
				expected, reason = long, "quota_exhausted"
			}
			limits := current.Extra["model_rate_limits"].(map[string]any)
			entry := limits["gemini-3.8-flash-high"].(map[string]any)
			require.Equal(t, expected.Format(time.RFC3339), entry["rate_limit_reset_at"])
			require.Equal(t, reason, entry["reason"])
			require.Equal(t, entry, cache.accounts[account.ID].Extra["model_rate_limits"].(map[string]any)["gemini-3.8-flash-high"])
			require.NoError(t, repo.SetModelRateLimit(ctx, account.ID, "gemini-3.8-flash-high", long.Add(time.Hour), "new_quota"))
			current, err = repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			entry = current.Extra["model_rate_limits"].(map[string]any)["gemini-3.8-flash-high"].(map[string]any)
			require.Equal(t, long.Add(time.Hour).Format(time.RFC3339), entry["rate_limit_reset_at"])
		})
	}
}

func TestAntigravityModelCooldownRepository_ConcurrentWritesKeepHardQuota(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	account := mustCreateAccount(t, client, &service.Account{Name: "concurrent-ag-cooldown", Platform: service.PlatformAntigravity})
	t.Cleanup(func() { _ = client.Account.DeleteOneID(account.ID).Exec(ctx) })
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	long := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	short := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	var writes sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		resetAt := short
		if i == 7 {
			resetAt = long
		}
		writes.Add(1)
		go func() {
			defer writes.Done()
			<-start
			errs <- repo.SetModelRateLimit(ctx, account.ID, "gemini-4-argon-high", resetAt)
		}()
	}
	close(start)
	writes.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	current, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	entry := current.Extra["model_rate_limits"].(map[string]any)["gemini-4-argon-high"].(map[string]any)
	require.Equal(t, long.Format(time.RFC3339), entry["rate_limit_reset_at"])
}

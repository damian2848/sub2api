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

func geminiHealthOutboxCount(t *testing.T, repo *accountRepository, id int64) int {
	t.Helper()
	var count int
	require.NoError(t, scanSingleRow(context.Background(), repo.sql,
		"SELECT COUNT(*) FROM scheduler_outbox WHERE event_type = $1 AND account_id = $2",
		[]any{service.SchedulerOutboxEventAccountChanged, id}, &count))
	return count
}

func TestGeminiWebHealthCooldownRepository_GenerationAndMonotonicUpdate(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	cache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, cache)
	account := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "gemini-web-health-generation@example.test", Platform: service.PlatformGemini, Type: service.AccountTypeGeminiWeb,
		Credentials: map[string]any{"gemini_web_cookie": "new-cookie", "gemini_web_cookie_generation": "current"},
	})
	initialEvents := geminiHealthOutboxCount(t, repo, account.ID)
	now := time.Now().UTC().Truncate(time.Second)
	longUntil, shortUntil := now.Add(10*time.Minute), now.Add(time.Minute)
	const longReason = "existing longer temporary cooldown"
	const healthReason = "Gemini Web repeated upstream failures; level=0"

	applied, err := repo.SetGeminiWebTempUnschedulable(ctx, account.ID, "old", longUntil, healthReason)
	require.NoError(t, err)
	require.False(t, applied)
	require.Empty(t, cache.setAccounts)
	require.Equal(t, initialEvents, geminiHealthOutboxCount(t, repo, account.ID))
	current, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, current.TempUnschedulableUntil)
	require.Equal(t, "new-cookie", current.GetCredential("gemini_web_cookie"))

	applied, err = repo.SetGeminiWebTempUnschedulable(ctx, account.ID, "current", longUntil, longReason)
	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, cache.setAccounts, 1)
	require.True(t, cache.accounts[account.ID].TempUnschedulableUntil.Equal(longUntil))
	require.Equal(t, longReason, cache.accounts[account.ID].TempUnschedulableReason)
	require.Equal(t, initialEvents+1, geminiHealthOutboxCount(t, repo, account.ID))

	for _, until := range []time.Time{shortUntil, longUntil} {
		applied, err = repo.SetGeminiWebTempUnschedulable(ctx, account.ID, "current", until, healthReason)
		require.NoError(t, err)
		require.False(t, applied, "shorter or equal health cooldown must preserve the existing reason and deadline")
	}
	require.Len(t, cache.setAccounts, 1)
	require.Equal(t, initialEvents+1, geminiHealthOutboxCount(t, repo, account.ID))
	current, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.True(t, current.TempUnschedulableUntil.Equal(longUntil))
	require.Equal(t, longReason, current.TempUnschedulableReason)

	extended := longUntil.Add(time.Minute)
	applied, err = repo.SetGeminiWebTempUnschedulable(ctx, account.ID, "current", extended, healthReason)
	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, cache.setAccounts, 2)
	require.Equal(t, initialEvents+2, geminiHealthOutboxCount(t, repo, account.ID))
	require.True(t, cache.accounts[account.ID].TempUnschedulableUntil.Equal(extended))
	require.Equal(t, healthReason, cache.accounts[account.ID].TempUnschedulableReason)

	// A newly constructed repository can recover the same blocking state from
	// PostgreSQL, without relying on the process-local health tracker.
	restarted := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	persisted, err := restarted.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.True(t, persisted.TempUnschedulableUntil.Equal(extended))
	require.Equal(t, healthReason, persisted.TempUnschedulableReason)
	require.False(t, persisted.IsSchedulable())
}

func TestGeminiWebHealthCooldownRepository_RecoveryRejectsOldInFlightFailure(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	cache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, cache)
	account := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "gemini-web-health-recovery@example.test", Platform: service.PlatformGemini, Type: service.AccountTypeGeminiWeb,
		Credentials: map[string]any{
			"gemini_web_cookie": "old-cookie", "gemini_web_cookie_generation": "old",
			"model_mapping": map[string]any{"gemini-4-argon": "gemini-4-argon"},
		},
	})
	until := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)
	applied, err := repo.SetGeminiWebTempUnschedulable(ctx, account.ID, "old", until, service.GeminiWebCookieRotationRejected)
	require.NoError(t, err)
	require.True(t, applied)
	expected, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)

	applied, err = repo.ApplyGeminiWebCookieRecovery(ctx, expected, "replacement-cookie", "replacement", time.Now().Add(time.Minute).UnixMilli())
	require.NoError(t, err)
	require.True(t, applied)
	current, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, current.TempUnschedulableUntil)
	require.Empty(t, current.TempUnschedulableReason)
	require.Equal(t, "replacement-cookie", current.GetCredential("gemini_web_cookie"))
	require.Equal(t, "replacement", current.GetCredential("gemini_web_cookie_generation"))
	require.Equal(t, account.Credentials["model_mapping"], current.Credentials["model_mapping"])
	require.Nil(t, cache.accounts[account.ID].TempUnschedulableUntil)
	require.Equal(t, "replacement", cache.accounts[account.ID].GetCredential("gemini_web_cookie_generation"))
	eventsAfterRecovery, snapshotsAfterRecovery := geminiHealthOutboxCount(t, repo, account.ID), len(cache.setAccounts)

	applied, err = repo.SetGeminiWebTempUnschedulable(ctx, account.ID, "old", until.Add(time.Hour), "Gemini Web repeated upstream failures; level=1")
	require.NoError(t, err)
	require.False(t, applied)
	applied, err = repo.UpdateGeminiWebCredentials(ctx, account.ID, "old", map[string]any{"gemini_web_cookie": "late-old-cookie"})
	require.NoError(t, err)
	require.False(t, applied)
	require.Equal(t, eventsAfterRecovery, geminiHealthOutboxCount(t, repo, account.ID))
	require.Len(t, cache.setAccounts, snapshotsAfterRecovery)
	current, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, current.TempUnschedulableUntil)
	require.Equal(t, "replacement-cookie", current.GetCredential("gemini_web_cookie"))

	applied, err = repo.SetGeminiWebTempUnschedulable(ctx, account.ID, "replacement", until, "Gemini Web repeated upstream failures; level=0")
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, eventsAfterRecovery+1, geminiHealthOutboxCount(t, repo, account.ID))
	require.Len(t, cache.setAccounts, snapshotsAfterRecovery+1)
	require.True(t, cache.accounts[account.ID].TempUnschedulableUntil.Equal(until))
}

func TestGeminiWebHealthCooldownRepository_ConcurrentWritesKeepCurrentLongestLimit(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	account := mustCreateAccount(t, client, &service.Account{
		Name: "gemini-web-health-concurrent@example.test", Platform: service.PlatformGemini, Type: service.AccountTypeGeminiWeb,
		Credentials: map[string]any{"gemini_web_cookie": "new-cookie", "gemini_web_cookie_generation": "current"},
	})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id=$1", account.ID)
		_ = client.Account.DeleteOneID(account.ID).Exec(ctx)
	})
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	now := time.Now().UTC().Truncate(time.Second)
	longUntil := now.Add(10 * time.Minute)
	type writeResult struct {
		stale   bool
		applied bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan writeResult, 16)
	var workers sync.WaitGroup
	for i := range 16 {
		generation, until := "current", now.Add(time.Minute)
		if i == 0 {
			until = longUntil
		} else if i%2 == 1 {
			generation, until = "old", now.Add(24*time.Hour)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			applied, err := repo.SetGeminiWebTempUnschedulable(ctx, account.ID, generation, until, "health")
			results <- writeResult{generation == "old", applied, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for result := range results {
		require.NoError(t, result.err)
		if result.stale {
			require.False(t, result.applied, "even a longer deadline from an old cookie must be rejected")
		}
	}
	current, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.True(t, current.TempUnschedulableUntil.Equal(longUntil))
	require.Equal(t, "current", current.GetCredential("gemini_web_cookie_generation"))
}

//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAntigravityPlanlensSQLGenerationAndManualState(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	cache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, tx, cache)
	account := mustCreateAccount(t, client, &service.Account{
		Name: "ant-import-manual-state", Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
		Status: service.StatusDisabled, Extra: map[string]any{"manual": true},
		Credentials: map[string]any{"access_token": "old", "planlens_oauth_generation": "old", "model_mapping": map[string]any{"manual": "custom"}},
	})
	_, err := client.Account.UpdateOneID(account.ID).SetSchedulable(false).Save(ctx)
	require.NoError(t, err)
	observed, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.NoError(t, repo.ApplyAntigravityImportCredentials(ctx, observed, map[string]any{
		"access_token": "imported", "planlens_oauth_generation": "new",
	}, time.Now().Add(time.Minute).UnixMilli()))
	got, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, "imported", got.GetCredential("access_token"))
	require.Equal(t, service.StatusDisabled, got.Status)
	require.False(t, got.Schedulable)
	require.Equal(t, observed.Extra, got.Extra)
	require.Equal(t, observed.Credentials["model_mapping"], got.Credentials["model_mapping"])

	applied, err := repo.UpdateAntigravityCredentialsIfGeneration(ctx, account.ID, "old", map[string]any{"access_token": "late"})
	require.NoError(t, err)
	require.False(t, applied)
	require.Empty(t, cache.setAccounts)
	applied, err = repo.UpdateAntigravityCredentialsIfGeneration(ctx, account.ID, "new", map[string]any{
		"access_token": "rotated", "model_mapping": map[string]any{"forbidden": "change"},
	})
	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, cache.setAccounts, 1)
	require.Equal(t, "rotated", cache.setAccounts[0].GetCredential("access_token"))
	require.Equal(t, got.Credentials["model_mapping"], cache.setAccounts[0].Credentials["model_mapping"])
	require.False(t, cache.setAccounts[0].Schedulable)
	var count int
	require.NoError(t, scanSingleRow(ctx, tx, "SELECT count(*) FROM scheduler_outbox WHERE account_id=$1", []any{account.ID}, &count))
	require.Equal(t, 2, count, "only the import and matching refresh publish durable changes")
}

func TestAntigravityPlanlensSQLFailureGuards(t *testing.T) {
	for _, mode := range []string{"temporary", "permanent", "reauthorized", "manual_disabled", "proxy_changed"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			client := tx.Client()
			cache := &schedulerCacheRecorder{}
			repo := newAccountRepositoryWithSQL(client, tx, cache)
			account := mustCreateAccount(t, client, &service.Account{
				Name: "ant-import-failure-" + mode, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "observed"},
			})
			observed, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			switch mode {
			case "reauthorized":
				_, err = client.Account.UpdateOneID(account.ID).SetCredentials(map[string]any{"access_token": "replacement"}).Save(ctx)
			case "manual_disabled":
				_, err = client.Account.UpdateOneID(account.ID).SetSchedulable(false).Save(ctx)
			case "proxy_changed":
				proxy := mustCreateProxy(t, client, &service.Proxy{Name: "ant-import-other-proxy"})
				_, err = client.Account.UpdateOneID(account.ID).SetProxyID(proxy.ID).Save(ctx)
			}
			require.NoError(t, err)
			applied, err := repo.SetAntigravityOAuthFailureIfCredentialsUnchanged(ctx, observed, time.Now().Add(time.Minute), "observed failure", mode == "permanent")
			require.NoError(t, err)
			wantApplied := mode == "temporary" || mode == "permanent"
			require.Equal(t, wantApplied, applied)
			got, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			var count int
			require.NoError(t, scanSingleRow(ctx, tx, "SELECT count(*) FROM scheduler_outbox WHERE account_id=$1", []any{account.ID}, &count))
			if wantApplied {
				require.Equal(t, 1, count)
				require.Len(t, cache.setAccounts, 1)
				if mode == "permanent" {
					require.Equal(t, service.StatusError, got.Status)
					require.False(t, got.Schedulable)
				} else {
					require.NotNil(t, got.TempUnschedulableUntil)
					require.Equal(t, "observed failure", got.TempUnschedulableReason)
				}
			} else {
				require.Zero(t, count)
				require.Empty(t, cache.setAccounts)
				require.Equal(t, service.StatusActive, got.Status)
				require.Nil(t, got.TempUnschedulableUntil)
			}
		})
	}
}

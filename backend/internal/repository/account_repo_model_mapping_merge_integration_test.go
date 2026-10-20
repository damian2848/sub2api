//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountModelMappingMergePreservesEachAccountsCredentialsAndAliases(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	accounts := make([]*service.Account, 0, 2)
	for i := 1; i <= 2; i++ {
		accounts = append(accounts, mustCreateAccount(t, tx.Client(), &service.Account{
			Name: fmt.Sprintf("mapping-merge-%d", i), Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
			Credentials: map[string]any{
				"access_token": fmt.Sprintf("fixture-token-%d", i),
				"project_id":   fmt.Sprintf("fixture-project-%d", i),
				"model_mapping": map[string]any{
					"existing-model": "existing-model",
					"shared-alias":   fmt.Sprintf("account-%d-target", i),
					"updated-alias":  "old-target",
				},
				"custom_config": map[string]any{"keep": true},
			},
		}))
	}
	patch := map[string]any{
		"shared-alias":  "shared-alias", // whitelist alone must not erase either existing alias
		"updated-alias": "new-target",   // an explicit alias edit must replace the old target
		"new-model":     "new-model",
	}
	rows, err := repo.BulkUpdate(ctx, []int64{accounts[0].ID, accounts[1].ID}, service.AccountBulkUpdate{
		MergeModelMapping: true,
		Credentials:       map[string]any{"model_mapping": patch, "intercept_warmup_requests": true},
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), rows)
	for i, account := range accounts {
		got, err := repo.GetByID(ctx, account.ID)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("fixture-token-%d", i+1), got.Credentials["access_token"])
		require.Equal(t, fmt.Sprintf("fixture-project-%d", i+1), got.Credentials["project_id"])
		require.Equal(t, map[string]any{"keep": true}, got.Credentials["custom_config"])
		require.Equal(t, true, got.Credentials["intercept_warmup_requests"])
		require.Equal(t, map[string]any{
			"existing-model": "existing-model",
			"shared-alias":   fmt.Sprintf("account-%d-target", i+1),
			"updated-alias":  "new-target",
			"new-model":      "new-model",
		}, got.Credentials["model_mapping"], "custom mappings must not unexpectedly gain system defaults")
	}
}

func TestAccountModelMappingMergeStartsFromDefaultsOnlyWhenNoCustomMapping(t *testing.T) {
	for _, existing := range []struct {
		name    string
		present bool
		value   any
	}{
		{name: "missing"},
		{name: "empty", present: true, value: map[string]any{}},
		{name: "null", present: true},
	} {
		t.Run(existing.name, func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			credentials := map[string]any{"access_token": "fixture-token"}
			if existing.present {
				credentials["model_mapping"] = existing.value
			}
			account := mustCreateAccount(t, tx.Client(), &service.Account{
				Name: "mapping-default-base", Platform: service.PlatformAntigravity,
				Type: service.AccountTypeOAuth, Credentials: credentials,
			})
			rows, err := repo.BulkUpdate(ctx, []int64{account.ID}, service.AccountBulkUpdate{
				MergeModelMapping: true,
				Credentials: map[string]any{"model_mapping": map[string]any{
					"new-alias":      "gemini-4-argon-low",
					"gemini-4-argon": "gemini-4-argon",
				}},
			})
			require.NoError(t, err)
			require.Equal(t, int64(1), rows)
			got, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			want := make(map[string]any, len(domain.DefaultAntigravityModelMapping)+1)
			for from, to := range domain.DefaultAntigravityModelMapping {
				want[from] = to
			}
			want["new-alias"] = "gemini-4-argon-low"
			require.NotEqual(t, "gemini-4-argon", want["gemini-4-argon"], "fixture must exercise default alias priority")
			require.Equal(t, want, got.Credentials["model_mapping"])
			require.Equal(t, "fixture-token", got.Credentials["access_token"])
		})
	}
}

func TestAccountModelMappingMergeEmptyPatchIsNoOpForMapping(t *testing.T) {
	for _, existing := range []struct {
		name    string
		present bool
		value   any
	}{
		{name: "missing"},
		{name: "empty", present: true, value: map[string]any{}},
		{name: "null", present: true},
		{name: "custom", present: true, value: map[string]any{"custom": "target"}},
	} {
		t.Run(existing.name, func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			credentials := map[string]any{"access_token": "fixture-token"}
			if existing.present {
				credentials["model_mapping"] = existing.value
			}
			account := mustCreateAccount(t, tx.Client(), &service.Account{
				Name: "mapping-empty-merge", Platform: service.PlatformAntigravity,
				Type: service.AccountTypeOAuth, Credentials: credentials,
			})
			_, err := repo.BulkUpdate(ctx, []int64{account.ID}, service.AccountBulkUpdate{
				MergeModelMapping: true,
				Credentials:       map[string]any{"model_mapping": map[string]any{}, "intercept_warmup_requests": true},
			})
			require.NoError(t, err)
			got, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			gotMapping, present := got.Credentials["model_mapping"]
			require.Equal(t, existing.present, present)
			require.Equal(t, existing.value, gotMapping)
			require.Equal(t, "fixture-token", got.Credentials["access_token"])
			require.Equal(t, true, got.Credentials["intercept_warmup_requests"])
		})
	}
}

func TestAccountModelMappingReplaceRetainsLegacyWholeMappingReplacement(t *testing.T) {
	for _, patch := range []map[string]any{
		{"new-model": "new-model", "new-alias": "target"},
		{},
	} {
		t.Run(fmt.Sprintf("entries=%d", len(patch)), func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			account := mustCreateAccount(t, tx.Client(), &service.Account{
				Name: "mapping-replace", Platform: service.PlatformAntigravity,
				Type: service.AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "fixture-token", "model_mapping": map[string]any{"old-alias": "old-target"},
				},
			})
			_, err := repo.BulkUpdate(ctx, []int64{account.ID}, service.AccountBulkUpdate{
				Credentials: map[string]any{"model_mapping": patch},
			})
			require.NoError(t, err)
			got, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.Equal(t, patch, got.Credentials["model_mapping"])
			require.Equal(t, "fixture-token", got.Credentials["access_token"])
		})
	}
}

func TestAccountModelMappingMergeConcurrentDisjointUpdatesAreNotLost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := testEntClient(t)
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	account := mustCreateAccount(t, client, &service.Account{
		Name: "mapping-concurrent-merge", Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "fixture-token", "model_mapping": map[string]any{"original": "original-target"},
		},
	})
	t.Cleanup(func() { _ = client.Account.DeleteOneID(account.ID).Exec(context.Background()) })

	// Hold the row until all independent writers are waiting in PostgreSQL. Each
	// UPDATE must then merge against the row produced by its predecessor.
	lock, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = lock.Rollback() }()
	var id int64
	require.NoError(t, lock.QueryRowContext(ctx, "SELECT id FROM accounts WHERE id=$1 FOR UPDATE", account.ID).Scan(&id))
	const writers = 6
	results := make(chan error, writers)
	var ready sync.WaitGroup
	ready.Add(writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		go func(i int) {
			ready.Done()
			<-start
			_, err := repo.BulkUpdate(ctx, []int64{account.ID}, service.AccountBulkUpdate{
				MergeModelMapping: true,
				Credentials: map[string]any{"model_mapping": map[string]any{
					fmt.Sprintf("alias-%d", i): fmt.Sprintf("target-%d", i),
				}},
			})
			results <- err
		}(i)
	}
	ready.Wait()
	close(start)
	require.Eventually(t, func() bool {
		var waiting int
		err := integrationDB.QueryRowContext(ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'UPDATE accounts SET credentials =%'",
		).Scan(&waiting)
		return err == nil && waiting >= writers
	}, 5*time.Second, 10*time.Millisecond, "all writers should overlap on the same locked row")
	require.NoError(t, lock.Commit())
	for i := 0; i < writers; i++ {
		require.NoError(t, <-results)
	}
	got, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	want := map[string]any{"original": "original-target"}
	for i := 0; i < writers; i++ {
		want[fmt.Sprintf("alias-%d", i)] = fmt.Sprintf("target-%d", i)
	}
	require.Equal(t, want, got.Credentials["model_mapping"])
	require.Equal(t, "fixture-token", got.Credentials["access_token"])
}

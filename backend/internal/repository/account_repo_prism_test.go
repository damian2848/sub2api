package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func prismMergeRows(extra string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "opencode_group_unchanged", "opencode_auto", "opencode_snapshot", "current_extra"}).
		AddRow(true, false, true, nil, nil, nil, nil, nil, nil, false, nil, nil, []byte(extra))
}

func TestLockAndMergeAccountExtraPreservesLatestPrismState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	current := `{"provider_preset":"prism_browser","prism_source_account_id":32,"prism_phase":"ready","prism_auto_enable_pending":false,"prism_models":["gpt-6.1-sol"],"prism_last_heartbeat_at":"2026-10-01T00:00:00Z","openai_responses_mode":"force_responses","openai_passthrough":false,"openai_apikey_responses_websockets_v2_mode":"off","old_admin_setting":true}`
	account := &service.Account{
		ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "prism-test"},
		Extra: map[string]any{
			"provider_preset": service.PrismProviderPreset, service.PrismSourceAccountKey: int64(32),
			"prism_phase": "provisioning", "prism_auto_enable_pending": true,
			"prism_error_code": "stale-error", "prism_injected": true,
			"openai_responses_mode": "force_chat_completions", "openai_passthrough": true,
			"openai_apikey_responses_websockets_v2_mode": "on", "new_admin_setting": true,
		},
	}
	mock.ExpectQuery(`(?s)SELECT.*FOR NO KEY UPDATE`).
		WithArgs(int64(41), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"prism-test"}`, nil).
		WillReturnRows(prismMergeRows(current))

	got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, nil, nil)
	require.NoError(t, err)
	var expected map[string]any
	require.NoError(t, json.Unmarshal([]byte(current), &expected))
	delete(expected, "old_admin_setting")
	expected["new_admin_setting"] = true
	require.Equal(t, expected, got)
	require.Equal(t, "provisioning", account.Extra["prism_phase"], "merge must not mutate the stale input map")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateAccountPrismKeepsWorkerCredentialsAndScheduling(t *testing.T) {
	for _, managed := range []bool{true, false} {
		name := "ordinary"
		if managed {
			name = "managed_prism"
		}
		t.Run(name, func(t *testing.T) {
			var updateSQL string
			matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
				if strings.HasPrefix(actual, "UPDATE ") {
					updateSQL = actual
				}
				return sqlmock.QueryMatcherRegexp.Match(expected, actual)
			})
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			repo := newAccountRepositoryWithSQL(client, db, nil)
			account := &service.Account{
				ID: 41, Name: "renamed", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "prism-test"}, Extra: map[string]any{},
				Concurrency: 1, Status: service.StatusActive, Schedulable: false,
			}
			current := `{}`
			if managed {
				account.Extra["provider_preset"] = service.PrismProviderPreset
				account.Extra[service.PrismSourceAccountKey] = int64(32)
				account.Extra["prism_phase"] = "provisioning"
				current = `{"provider_preset":"prism_browser","prism_source_account_id":32,"prism_phase":"ready","prism_auto_enable_pending":false}`
			}

			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT.*FOR NO KEY UPDATE`).
				WithArgs(int64(41), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"prism-test"}`, nil).
				WillReturnRows(prismMergeRows(current))
			mock.ExpectExec(`(?s)UPDATE .*accounts.*SET.*WHERE .*id.*`).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "id" = \$1`).
				WithArgs(int64(41)).WillReturnRows(updatedAccountRows(41, current))
			mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()

			require.NoError(t, repo.Update(context.Background(), account))
			require.NotEmpty(t, updateSQL)
			if managed {
				require.NotContains(t, updateSQL, `"credentials"`)
				require.NotContains(t, updateSQL, `"schedulable"`)
				require.Equal(t, "ready", account.Extra["prism_phase"])
				require.Equal(t, false, account.Extra["prism_auto_enable_pending"])
				require.Equal(t, map[string]any{"api_key": "sk-test"}, account.Credentials)
				require.True(t, account.Schedulable, "return the worker's current scheduling state")
			} else {
				require.Contains(t, updateSQL, `"credentials"`)
				require.Contains(t, updateSQL, `"schedulable"`)
				require.Equal(t, map[string]any{"api_key": "prism-test"}, account.Credentials)
				require.False(t, account.Schedulable)
			}
			require.Contains(t, updateSQL, `"name"`)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

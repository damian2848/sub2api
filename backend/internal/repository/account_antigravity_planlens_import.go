package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The short transaction serializes identity/configuration checks and writes with
// all existing account/group/proxy mutations. It performs no network requests.
// It provides atomic upserts for this API; unrelated legacy creation endpoints
// still do not impose global email uniqueness on the accounts table.
func (r *accountRepository) WithAntigravityImportTransaction(ctx context.Context, fn func(service.AccountRepository, service.GroupRepository, service.ProxyRepository) (int64, error)) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	if _, err := client.ExecContext(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return err
	}
	if _, err := client.ExecContext(ctx, "LOCK TABLE accounts, account_groups, groups, proxies IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return errors.New("busy")
	}
	accountRepo := &accountRepository{client: client, sql: client}
	id, err := fn(accountRepo, newGroupRepositoryWithSQL(client, client), newProxyRepositoryWithSQL(client, client))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.New("expired")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The transactional outbox also retries cache synchronization on failures.
	if err := r.syncGeminiRecoverySnapshot(ctx, id); err != nil {
		return errors.New("verification_failed")
	}
	return nil
}

func (r *accountRepository) ApplyAntigravityImportCredentials(ctx context.Context, expected *service.Account, patch map[string]any, deadline int64) error {
	previous, err := json.Marshal(expected.Credentials)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	result, err := r.sql.ExecContext(ctx, `WITH updated AS (
 UPDATE accounts SET credentials=credentials || $3::jsonb,updated_at=NOW(),
 status=CASE WHEN $6 THEN 'active' ELSE status END,error_message=CASE WHEN $6 THEN '' ELSE error_message END
 WHERE id=$1 AND deleted_at IS NULL AND platform='antigravity' AND type='oauth'
 AND credentials=$2::jsonb AND status=$7 AND coalesce(error_message,'')=$8 AND schedulable=$9
 AND (extract(epoch FROM clock_timestamp())*1000)<$4 RETURNING id
 ) INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload) SELECT $5,id,NULL,NULL FROM updated`, expected.ID, string(previous), string(payload), deadline, service.SchedulerOutboxEventAccountChanged, service.AntigravityPlanlensRecoverableOAuthError(expected, time.Now()), expected.Status, expected.ErrorMessage, expected.Schedulable)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("stale_revision")
	}
	return nil
}

// A refresh started before manual reauthorization cannot replace the new tokens.
// Merge only authentication keys; preserve administrator model mappings/settings.
func (r *accountRepository) UpdateAntigravityCredentialsIfGeneration(ctx context.Context, id int64, generation string, credentials map[string]any) (bool, error) {
	patch := map[string]any{}
	for _, key := range []string{"access_token", "refresh_token", "expires_at", "token_type", "project_id", "email", "_token_version"} {
		if value, ok := credentials[key]; ok {
			patch[key] = value
		}
	}
	payload, err := json.Marshal(patch)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `WITH updated AS (UPDATE accounts SET credentials=credentials || $3::jsonb,updated_at=NOW()
 WHERE id=$1 AND deleted_at IS NULL AND platform='antigravity' AND type='oauth'
 AND coalesce(credentials->>'planlens_oauth_generation','')=$2 RETURNING id)
 INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload) SELECT $4,id,NULL,NULL FROM updated`, id, generation, string(payload), service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	return true, r.syncGeminiRecoverySnapshot(ctx, id)
}

// Failure evidence is valid only for the exact authorization used upstream.
// Do not disable a replacement authorization or override a manual disable.
func (r *accountRepository) SetAntigravityOAuthFailureIfCredentialsUnchanged(ctx context.Context, expected *service.Account, until time.Time, reason string, permanent bool) (bool, error) {
	previous, err := json.Marshal(expected.Credentials)
	if err != nil {
		return false, err
	}
	mutation := "temp_unschedulable_until=$3,temp_unschedulable_reason=$4"
	if permanent {
		mutation = "status='error',error_message=$4,schedulable=FALSE"
	}
	// $3 remains bound in both forms so the statement has one stable parameter list.
	query := `WITH updated AS (UPDATE accounts SET ` + mutation + `,updated_at=NOW()
 WHERE id=$1 AND credentials=$2::jsonb AND deleted_at IS NULL AND platform='antigravity' AND type='oauth'
 AND status='active' AND schedulable=TRUE AND $3::timestamptz IS NOT NULL
 AND proxy_id IS NOT DISTINCT FROM $6::bigint RETURNING id)
 INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload) SELECT $5,id,NULL,NULL FROM updated`
	result, err := r.sql.ExecContext(ctx, query, expected.ID, string(previous), until, reason, service.SchedulerOutboxEventAccountChanged, expected.ProxyID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	return true, r.syncGeminiRecoverySnapshot(ctx, expected.ID)
}

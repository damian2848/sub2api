package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The row predicate is a database compare-and-swap, not a read-then-write check.
// It preserves all unrelated credentials/configuration and targets one identity.
func (r *accountRepository) ApplyGeminiWebCookieRecovery(ctx context.Context, expected *service.Account, cookie, generation string, deadlineAt int64) (bool, error) {
	previous, err := json.Marshal(expected.Credentials)
	if err != nil {
		return false, err
	}
	patch, err := json.Marshal(map[string]any{"gemini_web_cookie": cookie, "gemini_web_cookie_generation": generation, "gemini_web_cookie_last_refresh": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
 WITH updated AS (
 UPDATE accounts SET credentials = credentials || $3::jsonb,
 temp_unschedulable_until = NULL, temp_unschedulable_reason = NULL, updated_at = NOW()
 WHERE id = $1 AND deleted_at IS NULL AND platform = 'gemini' AND type = 'gemini_web'
 AND credentials = $2::jsonb AND name = $4 AND status = 'active' AND schedulable = TRUE
 AND coalesce(error_message, '') = '' AND temp_unschedulable_reason = $5
 AND (extract(epoch FROM clock_timestamp()) * 1000) < $6
 AND coalesce(extra->>'email', '') = $7
 AND NOT EXISTS (
 SELECT 1 FROM accounts duplicate WHERE duplicate.id <> accounts.id AND duplicate.deleted_at IS NULL
 AND duplicate.platform = 'gemini' AND duplicate.type = 'gemini_web'
 AND lower(coalesce(nullif(trim(duplicate.credentials->>'email'), ''), nullif(trim(duplicate.extra->>'email'), ''), regexp_replace(regexp_replace(trim(duplicate.name), '(?i)^gemini-web-', ''), '\.json$', '')))
   = lower(coalesce(nullif(trim(accounts.credentials->>'email'), ''), nullif(trim(accounts.extra->>'email'), ''), regexp_replace(regexp_replace(trim(accounts.name), '(?i)^gemini-web-', ''), '\.json$', '')))
 ) RETURNING id
 ) INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
 SELECT $8, id, NULL, NULL FROM updated
 `, expected.ID, string(previous), string(patch), expected.Name, expected.TempUnschedulableReason, deadlineAt, stringExtra(expected.Extra, "email"), service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	return true, r.syncGeminiRecoverySnapshot(ctx, expected.ID)
}

func stringExtra(extra map[string]any, key string) string {
	value, _ := extra[key].(string)
	return value
}

func (r *accountRepository) syncGeminiRecoverySnapshot(ctx context.Context, id int64) error {
	if r.schedulerCache == nil {
		return nil
	}
	account, err := r.GetByID(ctx, id)
	if err != nil {
		return err
	}
	return r.schedulerCache.SetAccount(ctx, account)
}

// Background work started before a recovery retains its old generation and
// cannot overwrite the replacement cookie when its network request completes.
func (r *accountRepository) UpdateGeminiWebCredentials(ctx context.Context, id int64, generation string, credentials map[string]any) (bool, error) {
	patch := map[string]any{}
	for _, key := range []string{"gemini_web_cookie", "gemini_web_cookie_last_refresh"} {
		if value, ok := credentials[key]; ok {
			patch[key] = value
		}
	}
	payload, err := json.Marshal(patch)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `WITH updated AS (UPDATE accounts SET credentials = credentials || $3::jsonb, updated_at = NOW()
 WHERE id = $1 AND deleted_at IS NULL AND platform = 'gemini' AND type = 'gemini_web'
 AND coalesce(credentials->>'gemini_web_cookie_generation', '') = $2 RETURNING id)
 INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload) SELECT $4,id,NULL,NULL FROM updated`, id, generation, string(payload), service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	return true, r.syncGeminiRecoverySnapshot(ctx, id)
}

func (r *accountRepository) SetGeminiWebTempUnschedulable(ctx context.Context, id int64, generation string, until time.Time, reason string) (bool, error) {
	result, err := r.sql.ExecContext(ctx, `WITH updated AS (UPDATE accounts SET temp_unschedulable_until=$3, temp_unschedulable_reason=$4, updated_at=NOW()
 WHERE id=$1 AND deleted_at IS NULL AND platform='gemini' AND type='gemini_web'
 AND coalesce(credentials->>'gemini_web_cookie_generation','')=$2 RETURNING id)
 INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload) SELECT $5,id,NULL,NULL FROM updated`, id, generation, until, reason, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	return true, r.syncGeminiRecoverySnapshot(ctx, id)
}

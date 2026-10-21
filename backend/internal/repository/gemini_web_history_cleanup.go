package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

var _ service.GeminiWebHistoryCleanupRepository = (*accountRepository)(nil)

// All paths take the account advisory lock before a job row lock. In
// particular, claim must not lock a candidate row before acquiring its account.
const geminiHistoryAccountLock = `SELECT pg_advisory_xact_lock(hashtextextended('gemini-web-history:' || $1::text, 0))`
const geminiHistoryAccountTryLock = `SELECT pg_try_advisory_xact_lock(hashtextextended('gemini-web-history:' || $1::text, 0))`

type geminiHistoryTxBeginner interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func (r *accountRepository) beginGeminiHistoryTx(ctx context.Context) (*sql.Tx, error) {
	db, ok := r.sql.(geminiHistoryTxBeginner)
	if !ok {
		return nil, errors.New("Gemini Web history repository requires a transactional SQL connection")
	}
	return db.BeginTx(ctx, nil)
}

func (r *accountRepository) EnqueueGeminiWebHistoryCleanup(ctx context.Context, accountID int64, identity string, windows []service.GeminiWebCleanupWindow) error {
	if accountID <= 0 || strings.TrimSpace(identity) == "" {
		return errors.New("invalid Gemini Web history cleanup identity")
	}
	// PostgreSQL timestamps have microsecond precision. Normalize before both
	// event lookups and JSON storage so nanosecond timestamps deduplicate too.
	normalized := make([]service.GeminiWebCleanupWindow, 0, len(windows))
	seen := make(map[string]bool)
	for _, window := range windows {
		if (window.ID != "five_hour" && window.ID != "weekly") || window.ResetAt.IsZero() {
			return errors.New("invalid Gemini Web history cleanup window")
		}
		window.ResetAt = window.ResetAt.UTC().Truncate(time.Microsecond)
		key := window.ID + ":" + window.ResetAt.Format(time.RFC3339Nano)
		if !seen[key] {
			seen[key] = true
			normalized = append(normalized, window)
		}
	}
	if len(normalized) == 0 {
		return nil
	}
	tx, err := r.beginGeminiHistoryTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, geminiHistoryAccountLock, accountID); err != nil {
		return err
	}
	newWindows := make([]service.GeminiWebCleanupWindow, 0, len(normalized))
	for _, window := range normalized {
		var exists bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_web_history_cleanup_events WHERE account_id=$1 AND window_id=$2 AND reset_at=$3)`, accountID, window.ID, window.ResetAt).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
			newWindows = append(newWindows, window)
		}
	}
	// Do not create an empty orphan job when all observations were duplicates.
	if len(newWindows) == 0 {
		return tx.Commit()
	}
	var jobID int64
	var existingJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT id, windows FROM gemini_web_history_cleanup_jobs WHERE account_id=$1 AND identity=$2 AND status='pending' AND snapshot_ids IS NULL ORDER BY id LIMIT 1 FOR UPDATE`, accountID, identity).Scan(&jobID, &existingJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var combined []service.GeminiWebCleanupWindow
	if err == nil {
		if err = json.Unmarshal(existingJSON, &combined); err != nil {
			return errors.New("invalid Gemini Web history cleanup windows")
		}
	}
	combined = append(combined, newWindows...)
	payload, err := json.Marshal(combined)
	if err != nil {
		return err
	}
	if jobID == 0 {
		err = tx.QueryRowContext(ctx, `INSERT INTO gemini_web_history_cleanup_jobs(account_id,identity,windows) VALUES($1,$2,$3::jsonb) RETURNING id`, accountID, identity, string(payload)).Scan(&jobID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE gemini_web_history_cleanup_jobs SET windows=$2::jsonb, updated_at=clock_timestamp() WHERE id=$1`, jobID, string(payload))
	}
	if err != nil {
		return err
	}
	for _, window := range newWindows {
		// The account lock serializes all inserts. Any unexpected conflict rolls
		// back the whole transaction, including the new/merged job.
		_, err = tx.ExecContext(ctx, `INSERT INTO gemini_web_history_cleanup_events(account_id,window_id,reset_at,job_id) VALUES($1,$2,$3,$4)`, accountID, window.ID, window.ResetAt, jobID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

const geminiHistoryClaimCandidates = `SELECT j.id,j.account_id FROM gemini_web_history_cleanup_jobs j
 WHERE ((j.status='pending' AND j.next_attempt_at<=clock_timestamp()) OR (j.status='running' AND j.lease_until<=clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM gemini_web_history_cleanup_jobs active WHERE active.account_id=j.account_id AND active.status='running' AND active.lease_until>clock_timestamp())
 ORDER BY j.next_attempt_at,j.id LIMIT 32`

const geminiHistoryClaim = `UPDATE gemini_web_history_cleanup_jobs j
 SET status='running',attempts=attempts+1,lease_token=$3,lease_until=clock_timestamp()+make_interval(secs=>$4::double precision),updated_at=clock_timestamp()
 WHERE j.id=$1 AND j.account_id=$2
 AND ((j.status='pending' AND j.next_attempt_at<=clock_timestamp()) OR (j.status='running' AND j.lease_until<=clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM gemini_web_history_cleanup_jobs active WHERE active.account_id=j.account_id AND active.status='running' AND active.lease_until>clock_timestamp())
 RETURNING id,account_id,identity,windows,snapshot_ids,attempts,lease_token,lease_until`

func (r *accountRepository) ClaimGeminiWebHistoryCleanup(ctx context.Context, ttl time.Duration) (*service.GeminiWebHistoryCleanupJob, error) {
	if ttl <= 0 {
		return nil, errors.New("invalid Gemini Web history lease duration")
	}
	rows, err := r.sql.QueryContext(ctx, geminiHistoryClaimCandidates)
	if err != nil {
		return nil, err
	}
	type candidate struct{ id, accountID int64 }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.accountID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for _, c := range candidates {
		job, err := r.claimGeminiHistoryCandidate(ctx, c.id, c.accountID, ttl)
		if err != nil || job != nil {
			return job, err
		}
	}
	return nil, nil
}

func (r *accountRepository) claimGeminiHistoryCandidate(ctx context.Context, id, accountID int64, ttl time.Duration) (*service.GeminiWebHistoryCleanupJob, error) {
	tx, err := r.beginGeminiHistoryTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var locked bool
	if err = tx.QueryRowContext(ctx, geminiHistoryAccountTryLock, accountID).Scan(&locked); err != nil || !locked {
		return nil, err
	}
	var job service.GeminiWebHistoryCleanupJob
	var windows, snapshot []byte
	err = tx.QueryRowContext(ctx, geminiHistoryClaim, id, accountID, uuid.NewString(), ttl.Seconds()).Scan(&job.ID, &job.AccountID, &job.Identity, &windows, &snapshot, &job.Attempts, &job.LeaseToken, &job.LeaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(windows, &job.Windows) != nil {
		return nil, errors.New("invalid Gemini Web history cleanup windows")
	}
	job.SnapshotReady = snapshot != nil
	if job.SnapshotReady && json.Unmarshal(snapshot, &job.Snapshot) != nil {
		return nil, errors.New("invalid Gemini Web history cleanup snapshot")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

// No operation may revive an expired owner; every write uses wall-clock time
// and the exact job/account/token. Heartbeats must arrive before expiration.
const geminiHistoryFence = ` WHERE id=$1 AND account_id=$2 AND lease_token=$3 AND status='running' AND lease_until>clock_timestamp()`

func (r *accountRepository) RenewGeminiWebHistoryCleanup(ctx context.Context, job *service.GeminiWebHistoryCleanupJob, ttl time.Duration) error {
	if job == nil || ttl <= 0 {
		return service.ErrGeminiWebHistoryLeaseLost
	}
	var until time.Time
	err := scanSingleRow(ctx, r.sql, `UPDATE gemini_web_history_cleanup_jobs SET lease_until=clock_timestamp()+make_interval(secs=>$4::double precision),updated_at=clock_timestamp()`+geminiHistoryFence+` RETURNING lease_until`, []any{job.ID, job.AccountID, job.LeaseToken, ttl.Seconds()}, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrGeminiWebHistoryLeaseLost
	}
	if err == nil {
		job.LeaseUntil = until
	}
	return err
}

func (r *accountRepository) GeminiWebHistoryHasActiveRequests(ctx context.Context, job *service.GeminiWebHistoryCleanupJob) (bool, error) {
	if job == nil {
		return false, service.ErrGeminiWebHistoryLeaseLost
	}
	var active bool
	err := scanSingleRow(ctx, r.sql, `SELECT EXISTS(SELECT 1 FROM gemini_web_history_requests WHERE account_id=$2 AND expires_at>clock_timestamp()) FROM gemini_web_history_cleanup_jobs`+geminiHistoryFence, []any{job.ID, job.AccountID, job.LeaseToken}, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return false, service.ErrGeminiWebHistoryLeaseLost
	}
	return active, err
}

func (r *accountRepository) SaveGeminiWebHistorySnapshot(ctx context.Context, job *service.GeminiWebHistoryCleanupJob, ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	payload, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	err = r.writeGeminiHistoryJob(ctx, job, `UPDATE gemini_web_history_cleanup_jobs SET snapshot_ids=$4::jsonb,conversation_count=jsonb_array_length($4::jsonb),updated_at=clock_timestamp()`+geminiHistoryFence+` AND (snapshot_ids IS NULL OR snapshot_ids=$4::jsonb)`, string(payload))
	if err == nil {
		job.Snapshot = append([]string{}, ids...)
		job.SnapshotReady = true
	}
	return err
}

func (r *accountRepository) FinishGeminiWebHistoryCleanup(ctx context.Context, job *service.GeminiWebHistoryCleanupJob) error {
	return r.writeGeminiHistoryJob(ctx, job, `UPDATE gemini_web_history_cleanup_jobs SET status='completed',snapshot_ids=NULL,lease_token=NULL,lease_until=NULL,last_error_code='',completed_at=clock_timestamp(),updated_at=clock_timestamp()`+geminiHistoryFence+` AND snapshot_ids IS NOT NULL`)
}

func (r *accountRepository) RetryGeminiWebHistoryCleanup(ctx context.Context, job *service.GeminiWebHistoryCleanupJob, code string, next time.Time) error {
	return r.writeGeminiHistoryJob(ctx, job, `UPDATE gemini_web_history_cleanup_jobs SET status='pending',lease_token=NULL,lease_until=NULL,last_error_code=$4,next_attempt_at=$5,updated_at=clock_timestamp()`+geminiHistoryFence, geminiHistorySafeErrorCode(code), next)
}

func (r *accountRepository) CancelGeminiWebHistoryCleanup(ctx context.Context, job *service.GeminiWebHistoryCleanupJob, code string) error {
	return r.writeGeminiHistoryJob(ctx, job, `UPDATE gemini_web_history_cleanup_jobs SET status='cancelled',snapshot_ids=NULL,lease_token=NULL,lease_until=NULL,last_error_code=$4,completed_at=clock_timestamp(),updated_at=clock_timestamp()`+geminiHistoryFence, geminiHistorySafeErrorCode(code))
}

func (r *accountRepository) writeGeminiHistoryJob(ctx context.Context, job *service.GeminiWebHistoryCleanupJob, query string, extra ...any) error {
	if job == nil || job.LeaseToken == "" {
		return service.ErrGeminiWebHistoryLeaseLost
	}
	args := append([]any{job.ID, job.AccountID, job.LeaseToken}, extra...)
	result, err := r.sql.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return service.ErrGeminiWebHistoryLeaseLost
	}
	return err
}

func geminiHistorySafeErrorCode(code string) string {
	switch code {
	case "quota_unavailable", "quota_recovered", "quota_window_changed", "credential_changed", "account_unavailable", "active_requests", "history_list_failed", "history_delete_failed", "history_verify_failed", "lease_lost", "shutdown", "internal_error":
		return code
	default:
		return "internal_error"
	}
}

func (r *accountRepository) BeginGeminiWebHistoryRequest(ctx context.Context, accountID int64, ttl time.Duration) (string, error) {
	if accountID <= 0 || ttl <= 0 {
		return "", errors.New("invalid Gemini Web request lease")
	}
	tx, err := r.beginGeminiHistoryTx(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, geminiHistoryAccountLock, accountID); err != nil {
		return "", err
	}
	var blocked bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gemini_web_history_cleanup_jobs WHERE account_id=$1 AND status='running' AND lease_until>clock_timestamp())`, accountID).Scan(&blocked)
	if err != nil {
		return "", err
	}
	if blocked {
		return "", service.ErrGeminiWebHistoryMaintenance
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM gemini_web_history_requests WHERE account_id=$1 AND expires_at<=clock_timestamp()`, accountID); err != nil {
		return "", err
	}
	token := uuid.NewString()
	_, err = tx.ExecContext(ctx, `INSERT INTO gemini_web_history_requests(account_id,token,expires_at) VALUES($1,$2,clock_timestamp()+make_interval(secs=>$3::double precision))`, accountID, token, ttl.Seconds())
	if err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

func (r *accountRepository) EndGeminiWebHistoryRequest(ctx context.Context, accountID int64, token string) error {
	if accountID <= 0 || token == "" {
		return fmt.Errorf("invalid Gemini Web request lease")
	}
	_, err := r.sql.ExecContext(ctx, `DELETE FROM gemini_web_history_requests WHERE account_id=$1 AND token=$2`, accountID, token)
	return err
}

//go:build gemini_history_integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Run with a disposable local PostgreSQL database:
// GEMINI_HISTORY_TEST_DSN=postgres://postgres@127.0.0.1:PORT/gemini_history_test?sslmode=disable
// go test -tags gemini_history_integration ./internal/repository -run TestGeminiHistoryPostgres
// Each test uses an isolated schema; it never reads accounts or credentials.
func historyPostgresRepo(t *testing.T) (*accountRepository, *sql.DB) {
	t.Helper()
	raw := os.Getenv("GEMINI_HISTORY_TEST_DSN")
	if raw == "" {
		t.Skip("set GEMINI_HISTORY_TEST_DSN to a disposable local PostgreSQL database")
	}
	u, err := url.Parse(raw)
	require.NoError(t, err)
	require.Contains(t, []string{"127.0.0.1", "localhost", "::1"}, u.Hostname(), "history integration tests only allow local PostgreSQL")
	require.Equal(t, "/gemini_history_test", u.Path)
	admin, err := sql.Open("postgres", raw)
	require.NoError(t, err)
	schema := "gemini_history_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	db.SetMaxOpenConns(24)
	t.Cleanup(func() {
		_ = db.Close()
		_, dropErr := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, dropErr)
		_ = admin.Close()
	})
	_, err = db.Exec(`CREATE TABLE accounts(id BIGINT PRIMARY KEY); INSERT INTO accounts(id) SELECT generate_series(1,10)`)
	require.NoError(t, err)
	migration, err := os.ReadFile("../../migrations/287_gemini_web_history_cleanup.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err, "migration must be safe to reapply")
	return &accountRepository{sql: db}, db
}

func TestGeminiHistoryPostgresConcurrentEnqueueAndClaim(t *testing.T) {
	repo, db := historyPostgresRepo(t)
	ctx := context.Background()
	windows := []service.GeminiWebCleanupWindow{historyCleanupWindow("five_hour"), historyCleanupWindow("weekly")}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%3 == 0 {
				errs <- repo.EnqueueGeminiWebHistoryCleanup(ctx, 1, "principal", windows)
			} else {
				errs <- repo.EnqueueGeminiWebHistoryCleanup(ctx, 1, "principal", windows[i%2:i%2+1])
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var jobs, events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM gemini_web_history_cleanup_jobs`).Scan(&jobs))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM gemini_web_history_cleanup_events`).Scan(&events))
	require.Equal(t, 1, jobs, "dual-window observations coalesce without orphan jobs")
	require.Equal(t, 2, events)
	claimed := make(chan *service.GeminiWebHistoryCleanupJob, 16)
	errs = make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := repo.ClaimGeminiWebHistoryCleanup(ctx, time.Minute)
			claimed <- job
			errs <- err
		}()
	}
	wg.Wait()
	close(claimed)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var owner *service.GeminiWebHistoryCleanupJob
	for job := range claimed {
		if job != nil {
			require.Nil(t, owner, "only one worker can own an account")
			owner = job
		}
	}
	require.NotNil(t, owner)
	require.Len(t, owner.Windows, 2)
	require.False(t, owner.SnapshotReady)
	_, err := repo.BeginGeminiWebHistoryRequest(ctx, 1, time.Minute)
	require.ErrorIs(t, err, service.ErrGeminiWebHistoryMaintenance)
	require.NoError(t, repo.SaveGeminiWebHistorySnapshot(ctx, owner, nil))
	require.NoError(t, repo.FinishGeminiWebHistoryCleanup(ctx, owner))
	require.ErrorIs(t, repo.FinishGeminiWebHistoryCleanup(ctx, owner), service.ErrGeminiWebHistoryLeaseLost)
	require.NoError(t, repo.EnqueueGeminiWebHistoryCleanup(ctx, 1, "principal", windows))
	var snapshotNull bool
	require.NoError(t, db.QueryRow(`SELECT count(*),bool_and(snapshot_ids IS NULL) FROM gemini_web_history_cleanup_jobs`).Scan(&jobs, &snapshotNull))
	require.Equal(t, 1, jobs, "completion remains deduplicated")
	require.True(t, snapshotNull, "completed jobs retain no history IDs")
}

func TestGeminiHistoryPostgresAdmissionClaimRace(t *testing.T) {
	repo, _ := historyPostgresRepo(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		window := historyCleanupWindow("five_hour")
		window.ResetAt = window.ResetAt.Add(time.Duration(i) * time.Hour)
		require.NoError(t, repo.EnqueueGeminiWebHistoryCleanup(ctx, 1, "principal", []service.GeminiWebCleanupWindow{window}))
		start := make(chan struct{})
		type requestResult struct {
			token string
			err   error
		}
		type claimResult struct {
			job *service.GeminiWebHistoryCleanupJob
			err error
		}
		requests := make(chan requestResult, 1)
		claims := make(chan claimResult, 1)
		go func() {
			<-start
			token, err := repo.BeginGeminiWebHistoryRequest(ctx, 1, time.Minute)
			requests <- requestResult{token, err}
		}()
		go func() {
			<-start
			job, err := repo.ClaimGeminiWebHistoryCleanup(ctx, time.Minute)
			claims <- claimResult{job, err}
		}()
		close(start)
		r, c := <-requests, <-claims
		require.NoError(t, c.err)
		if c.job == nil { // Admission briefly owned the advisory lock; claim retries.
			c.job, c.err = repo.ClaimGeminiWebHistoryCleanup(ctx, time.Minute)
			require.NoError(t, c.err)
		}
		require.NotNil(t, c.job)
		active, err := repo.GeminiWebHistoryHasActiveRequests(ctx, c.job)
		require.NoError(t, err)
		if r.err == nil {
			require.True(t, active, "admitted request must be visible to cleanup")
			require.NoError(t, repo.EndGeminiWebHistoryRequest(ctx, 1, r.token))
		} else {
			require.ErrorIs(t, r.err, service.ErrGeminiWebHistoryMaintenance)
			require.False(t, active)
		}
		active, err = repo.GeminiWebHistoryHasActiveRequests(ctx, c.job)
		require.NoError(t, err)
		require.False(t, active)
		require.NoError(t, repo.SaveGeminiWebHistorySnapshot(ctx, c.job, []string{}))
		require.NoError(t, repo.FinishGeminiWebHistoryCleanup(ctx, c.job))
	}
}

func TestGeminiHistoryPostgresExpiredLeaseSnapshotRetryAndRequestExpiry(t *testing.T) {
	repo, db := historyPostgresRepo(t)
	ctx := context.Background()
	window := historyCleanupWindow("five_hour")
	require.NoError(t, repo.EnqueueGeminiWebHistoryCleanup(ctx, 1, "principal", []service.GeminiWebCleanupWindow{window}))
	old, err := repo.ClaimGeminiWebHistoryCleanup(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, old)
	_, err = db.Exec(`UPDATE gemini_web_history_cleanup_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID)
	require.NoError(t, err)
	require.ErrorIs(t, repo.RenewGeminiWebHistoryCleanup(ctx, old, time.Minute), service.ErrGeminiWebHistoryLeaseLost)
	token, err := repo.BeginGeminiWebHistoryRequest(ctx, 1, time.Minute)
	require.NoError(t, err, "expired cleanup leases cannot block admission forever")
	current, err := repo.ClaimGeminiWebHistoryCleanup(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, current)
	require.NotEqual(t, old.LeaseToken, current.LeaseToken)
	for _, op := range []func() error{
		func() error { return repo.RenewGeminiWebHistoryCleanup(ctx, old, time.Minute) },
		func() error { return repo.SaveGeminiWebHistorySnapshot(ctx, old, []string{"must-not-write"}) },
		func() error { return repo.FinishGeminiWebHistoryCleanup(ctx, old) },
		func() error { return repo.RetryGeminiWebHistoryCleanup(ctx, old, "shutdown", time.Now()) },
		func() error { return repo.CancelGeminiWebHistoryCleanup(ctx, old, "credential_changed") },
	} {
		require.ErrorIs(t, op(), service.ErrGeminiWebHistoryLeaseLost)
	}
	_, err = repo.GeminiWebHistoryHasActiveRequests(ctx, old)
	require.ErrorIs(t, err, service.ErrGeminiWebHistoryLeaseLost)
	require.NoError(t, repo.RenewGeminiWebHistoryCleanup(ctx, current, 2*time.Minute))
	active, err := repo.GeminiWebHistoryHasActiveRequests(ctx, current)
	require.NoError(t, err)
	require.True(t, active)
	require.NoError(t, repo.EndGeminiWebHistoryRequest(ctx, 1, "different-token"))
	active, err = repo.GeminiWebHistoryHasActiveRequests(ctx, current)
	require.NoError(t, err)
	require.True(t, active, "ending another token must not free an active request")
	_, err = db.Exec(`UPDATE gemini_web_history_requests SET expires_at=clock_timestamp()-interval '1 second' WHERE account_id=1 AND token=$1`, token)
	require.NoError(t, err)
	active, err = repo.GeminiWebHistoryHasActiveRequests(ctx, current)
	require.NoError(t, err)
	require.False(t, active, "crashed requests eventually drain")
	require.NoError(t, repo.SaveGeminiWebHistorySnapshot(ctx, current, []string{"c_first", "c_second"}))
	require.NoError(t, repo.SaveGeminiWebHistorySnapshot(ctx, current, []string{"c_first", "c_second"}))
	require.ErrorIs(t, repo.SaveGeminiWebHistorySnapshot(ctx, current, []string{"c_new"}), service.ErrGeminiWebHistoryLeaseLost, "a retry must never broaden the frozen deletion set")
	require.NoError(t, repo.RetryGeminiWebHistoryCleanup(ctx, current, "private upstream body", time.Now().Add(-time.Second)))
	var code string
	require.NoError(t, db.QueryRow(`SELECT last_error_code FROM gemini_web_history_cleanup_jobs WHERE id=$1`, current.ID).Scan(&code))
	require.Equal(t, "internal_error", code)
	require.NoError(t, repo.EnqueueGeminiWebHistoryCleanup(ctx, 1, "principal", []service.GeminiWebCleanupWindow{historyCleanupWindow("weekly")}))
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM gemini_web_history_cleanup_jobs`).Scan(&count))
	require.Equal(t, 2, count, "a new quota event must not merge into a frozen snapshot")
	resumed, err := repo.ClaimGeminiWebHistoryCleanup(ctx, time.Minute)
	require.NoError(t, err)
	require.Equal(t, current.ID, resumed.ID)
	require.True(t, resumed.SnapshotReady)
	require.Equal(t, []string{"c_first", "c_second"}, resumed.Snapshot)
	require.NoError(t, repo.FinishGeminiWebHistoryCleanup(ctx, resumed))
	second, err := repo.ClaimGeminiWebHistoryCleanup(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.NoError(t, repo.SaveGeminiWebHistorySnapshot(ctx, second, []string{"c_third"}))
	require.NoError(t, repo.CancelGeminiWebHistoryCleanup(ctx, second, "quota_recovered"))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM gemini_web_history_cleanup_jobs WHERE snapshot_ids IS NOT NULL`).Scan(&count))
	require.Zero(t, count)
	_, err = repo.BeginGeminiWebHistoryRequest(ctx, 1, time.Minute)
	require.False(t, errors.Is(err, service.ErrGeminiWebHistoryMaintenance))
	require.NoError(t, err)
}

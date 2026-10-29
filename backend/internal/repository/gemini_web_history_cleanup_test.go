package repository

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func historyCleanupRepo(t *testing.T) (*accountRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	})
	return &accountRepository{sql: db}, mock
}

func historyCleanupLock(mock sqlmock.Sqlmock, accountID int64) {
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(geminiHistoryAccountLock)).WithArgs(accountID).WillReturnResult(sqlmock.NewResult(0, 1))
}

func historyCleanupWindow(id string) service.GeminiWebCleanupWindow {
	return service.GeminiWebCleanupWindow{ID: id, ResetAt: time.Date(2026, 10, 21, 12, 0, 0, 123456789, time.UTC)}
}

func TestGeminiHistoryEnqueueDuplicateCreatesNoOrphan(t *testing.T) {
	repo, mock := historyCleanupRepo(t)
	w := historyCleanupWindow("five_hour")
	historyCleanupLock(mock, 243)
	mock.ExpectQuery("SELECT EXISTS.*gemini_web_history_cleanup_events").
		WithArgs(int64(243), w.ID, w.ResetAt.Truncate(time.Microsecond)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectCommit()
	// Duplicate input windows and duplicate observations must both be harmless.
	require.NoError(t, repo.EnqueueGeminiWebHistoryCleanup(context.Background(), 243, "identity", []service.GeminiWebCleanupWindow{w, w}))
}

type historyWindowsArgument struct {
	windows []service.GeminiWebCleanupWindow
}

func (a historyWindowsArgument) Match(value driver.Value) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	var windows []service.GeminiWebCleanupWindow
	if json.Unmarshal([]byte(s), &windows) != nil || len(windows) != len(a.windows) {
		return false
	}
	for i := range windows {
		if windows[i].ID != a.windows[i].ID || !windows[i].ResetAt.Equal(a.windows[i].ResetAt.UTC().Truncate(time.Microsecond)) {
			return false
		}
	}
	return true
}

func TestGeminiHistoryEnqueueCoalescesOnlyUnsnapshottedPendingJob(t *testing.T) {
	repo, mock := historyCleanupRepo(t)
	first, second := historyCleanupWindow("five_hour"), historyCleanupWindow("weekly")
	first.ResetAt = first.ResetAt.Truncate(time.Microsecond)
	existing, err := json.Marshal([]service.GeminiWebCleanupWindow{first})
	require.NoError(t, err)
	historyCleanupLock(mock, 243)
	mock.ExpectQuery("SELECT EXISTS.*gemini_web_history_cleanup_events").WithArgs(int64(243), second.ID, second.ResetAt.Truncate(time.Microsecond)).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("SELECT id, windows.*status='pending' AND snapshot_ids IS NULL.*FOR UPDATE").WithArgs(int64(243), "identity").WillReturnRows(sqlmock.NewRows([]string{"id", "windows"}).AddRow(12, existing))
	mock.ExpectExec("UPDATE gemini_web_history_cleanup_jobs SET windows").WithArgs(int64(12), historyWindowsArgument{[]service.GeminiWebCleanupWindow{first, second}}).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO gemini_web_history_cleanup_events").WithArgs(int64(243), second.ID, second.ResetAt.Truncate(time.Microsecond), int64(12)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	require.NoError(t, repo.EnqueueGeminiWebHistoryCleanup(context.Background(), 243, "identity", []service.GeminiWebCleanupWindow{second}))
}

func TestGeminiHistoryEnqueueEventFailureRollsBackNewJob(t *testing.T) {
	repo, mock := historyCleanupRepo(t)
	w := historyCleanupWindow("five_hour")
	historyCleanupLock(mock, 243)
	mock.ExpectQuery("SELECT EXISTS.*gemini_web_history_cleanup_events").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("SELECT id, windows.*FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"id", "windows"}))
	mock.ExpectQuery("INSERT INTO gemini_web_history_cleanup_jobs").WithArgs(int64(243), "identity", historyWindowsArgument{[]service.GeminiWebCleanupWindow{w}}).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(12))
	writeErr := errors.New("event write failed")
	mock.ExpectExec("INSERT INTO gemini_web_history_cleanup_events").WillReturnError(writeErr)
	mock.ExpectRollback()
	require.ErrorIs(t, repo.EnqueueGeminiWebHistoryCleanup(context.Background(), 243, "identity", []service.GeminiWebCleanupWindow{w}), writeErr)
}

func TestGeminiHistoryClaimSkipsBusyAccountAndRestoresEmptySnapshot(t *testing.T) {
	repo, mock := historyCleanupRepo(t)
	mock.ExpectQuery(regexp.QuoteMeta(geminiHistoryClaimCandidates)).WillReturnRows(sqlmock.NewRows([]string{"id", "account_id"}).AddRow(11, 242).AddRow(12, 243))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(geminiHistoryAccountTryLock)).WithArgs(int64(242)).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))
	mock.ExpectRollback()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(geminiHistoryAccountTryLock)).WithArgs(int64(243)).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	until := time.Now().Add(time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta(geminiHistoryClaim)).WithArgs(int64(12), int64(243), sqlmock.AnyArg(), float64(60)).WillReturnRows(sqlmock.NewRows([]string{"id", "account_id", "identity", "windows", "snapshot_ids", "attempts", "lease_token", "lease_until"}).AddRow(12, 243, "identity", `[]`, `[]`, 2, "new-owner", until))
	mock.ExpectCommit()
	job, err := repo.ClaimGeminiWebHistoryCleanup(context.Background(), time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(243), job.AccountID)
	require.Equal(t, "new-owner", job.LeaseToken)
	require.Equal(t, 2, job.Attempts)
	require.True(t, job.SnapshotReady, "an empty durable snapshot must not be mistaken for not-yet-listed history")
	require.Empty(t, job.Snapshot)
}

func TestGeminiHistoryBeginRequestBlockedByClaim(t *testing.T) {
	repo, mock := historyCleanupRepo(t)
	historyCleanupLock(mock, 243)
	mock.ExpectQuery("SELECT EXISTS.*status='running' AND lease_until>clock_timestamp").WithArgs(int64(243)).WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(true))
	mock.ExpectRollback()
	token, err := repo.BeginGeminiWebHistoryRequest(context.Background(), 243, 6*time.Minute)
	require.Empty(t, token)
	require.ErrorIs(t, err, service.ErrGeminiWebHistoryMaintenance)
}

func TestGeminiHistoryRequestLeaseLifecycle(t *testing.T) {
	repo, mock := historyCleanupRepo(t)
	historyCleanupLock(mock, 243)
	mock.ExpectQuery("SELECT EXISTS.*status='running' AND lease_until>clock_timestamp").WithArgs(int64(243)).WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
	mock.ExpectExec("DELETE FROM gemini_web_history_requests WHERE account_id=\\$1 AND expires_at<=clock_timestamp").WithArgs(int64(243)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO gemini_web_history_requests").WithArgs(int64(243), sqlmock.AnyArg(), float64(360)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	token, err := repo.BeginGeminiWebHistoryRequest(context.Background(), 243, 6*time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM gemini_web_history_requests WHERE account_id=$1 AND token=$2`)).WithArgs(int64(243), token).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.EndGeminiWebHistoryRequest(context.Background(), 243, token))
}

func TestGeminiHistoryExpiredWorkerCannotRenewObserveOrWrite(t *testing.T) {
	for _, operation := range []string{"renew", "active", "snapshot", "finish", "retry", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			repo, mock := historyCleanupRepo(t)
			oldUntil := time.Now().Add(-time.Minute)
			job := &service.GeminiWebHistoryCleanupJob{ID: 12, AccountID: 243, LeaseToken: "old-owner", LeaseUntil: oldUntil}
			fence := regexp.QuoteMeta(geminiHistoryFence)
			var err error
			switch operation {
			case "renew":
				mock.ExpectQuery("UPDATE gemini_web_history_cleanup_jobs SET lease_until.*"+fence).WithArgs(int64(12), int64(243), "old-owner", float64(60)).WillReturnRows(sqlmock.NewRows([]string{"lease_until"}))
				err = repo.RenewGeminiWebHistoryCleanup(context.Background(), job, time.Minute)
				require.Equal(t, oldUntil, job.LeaseUntil)
			case "active":
				mock.ExpectQuery("SELECT EXISTS.*"+fence).WithArgs(int64(12), int64(243), "old-owner").WillReturnRows(sqlmock.NewRows([]string{"exists"}))
				_, err = repo.GeminiWebHistoryHasActiveRequests(context.Background(), job)
			case "snapshot":
				mock.ExpectExec("UPDATE gemini_web_history_cleanup_jobs SET snapshot_ids.*"+fence).WithArgs(int64(12), int64(243), "old-owner", `[]`).WillReturnResult(sqlmock.NewResult(0, 0))
				err = repo.SaveGeminiWebHistorySnapshot(context.Background(), job, nil)
				require.False(t, job.SnapshotReady)
			case "finish":
				mock.ExpectExec("UPDATE gemini_web_history_cleanup_jobs SET status='completed',snapshot_ids=NULL.*"+fence).WithArgs(int64(12), int64(243), "old-owner").WillReturnResult(sqlmock.NewResult(0, 0))
				err = repo.FinishGeminiWebHistoryCleanup(context.Background(), job)
			case "retry":
				mock.ExpectExec("UPDATE gemini_web_history_cleanup_jobs SET status='pending'.*"+fence).WithArgs(int64(12), int64(243), "old-owner", "internal_error", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
				err = repo.RetryGeminiWebHistoryCleanup(context.Background(), job, "private response body must not be persisted", time.Now())
			case "cancel":
				mock.ExpectExec("UPDATE gemini_web_history_cleanup_jobs SET status='cancelled',snapshot_ids=NULL.*"+fence).WithArgs(int64(12), int64(243), "old-owner", "credential_changed").WillReturnResult(sqlmock.NewResult(0, 0))
				err = repo.CancelGeminiWebHistoryCleanup(context.Background(), job, "credential_changed")
			}
			require.ErrorIs(t, err, service.ErrGeminiWebHistoryLeaseLost)
		})
	}
}

func TestGeminiHistorySnapshotEmptyIsDurablyReady(t *testing.T) {
	repo, mock := historyCleanupRepo(t)
	job := &service.GeminiWebHistoryCleanupJob{ID: 12, AccountID: 243, LeaseToken: "owner"}
	mock.ExpectExec("UPDATE gemini_web_history_cleanup_jobs SET snapshot_ids.*AND \\(snapshot_ids IS NULL OR snapshot_ids=\\$4::jsonb\\)").WithArgs(int64(12), int64(243), "owner", `[]`).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.SaveGeminiWebHistorySnapshot(context.Background(), job, nil))
	require.True(t, job.SnapshotReady)
	require.NotNil(t, job.Snapshot)
}

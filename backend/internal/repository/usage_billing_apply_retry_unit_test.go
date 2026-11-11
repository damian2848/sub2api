//go:build unit

package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	usageBillingClaimSQL       = `(?s)INSERT INTO usage_billing_dedup \(request_id, api_key_id, request_fingerprint\)`
	usageBillingExistingFPSQL  = `(?s)SELECT request_fingerprint\s+FROM usage_billing_dedup\s+WHERE`
	usageBillingArchivedFPSQL  = `(?s)SELECT request_fingerprint\s+FROM usage_billing_dedup_archive`
	usageBillingRetryRequestID = "req-retry"
	usageBillingRetryAPIKeyID  = int64(77)
	usageBillingRetryFP        = "fp-retry"
)

func withFastUsageBillingRetries(t *testing.T) {
	t.Helper()
	prev := usageBillingApplyRetryBackoffs
	usageBillingApplyRetryBackoffs = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { usageBillingApplyRetryBackoffs = prev })
}

// newRetryTestCommand 不带任何扣费金额，Apply 只执行 dedup 认领 + 提交，便于用
// sqlmock 精确描述每一次尝试。
func newRetryTestCommand() *service.UsageBillingCommand {
	return &service.UsageBillingCommand{
		RequestID:          usageBillingRetryRequestID,
		APIKeyID:           usageBillingRetryAPIKeyID,
		RequestFingerprint: usageBillingRetryFP,
		UserID:             1,
		AccountID:          2,
	}
}

func expectClaimFails(mock sqlmock.Sqlmock, err error) {
	mock.ExpectBegin()
	mock.ExpectQuery(usageBillingClaimSQL).
		WithArgs(usageBillingRetryRequestID, usageBillingRetryAPIKeyID, usageBillingRetryFP).
		WillReturnError(err)
	mock.ExpectRollback()
}

func expectClaimSucceeds(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectQuery(usageBillingClaimSQL).
		WithArgs(usageBillingRetryRequestID, usageBillingRetryAPIKeyID, usageBillingRetryFP).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectQuery(usageBillingArchivedFPSQL).
		WithArgs(usageBillingRetryRequestID, usageBillingRetryAPIKeyID).
		WillReturnError(sql.ErrNoRows)
}

func TestUsageBillingApply_RetriesTransientErrorThenApplies(t *testing.T) {
	withFastUsageBillingRetries(t)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	expectClaimFails(mock, &pq.Error{Code: "40P01", Message: "deadlock detected"})
	expectClaimFails(mock, &pq.Error{Code: "08006", Message: "connection failure"})
	expectClaimSucceeds(mock)
	mock.ExpectCommit()

	repo := &usageBillingRepository{db: db}
	result, err := repo.Apply(context.Background(), newRetryTestCommand())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 第一次 Commit 实际已落库但返回了连接错误：重放命中 dedup（同一指纹），返回
// Applied=false 而不是再扣一次。
func TestUsageBillingApply_AmbiguousCommitRetryDoesNotDoubleCharge(t *testing.T) {
	withFastUsageBillingRetries(t)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	expectClaimSucceeds(mock)
	mock.ExpectCommit().WillReturnError(&pq.Error{Code: "08006", Message: "connection lost during commit"})

	mock.ExpectBegin()
	mock.ExpectQuery(usageBillingClaimSQL).
		WithArgs(usageBillingRetryRequestID, usageBillingRetryAPIKeyID, usageBillingRetryFP).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(usageBillingExistingFPSQL).
		WithArgs(usageBillingRetryRequestID, usageBillingRetryAPIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}).AddRow(usageBillingRetryFP))
	mock.ExpectRollback()

	repo := &usageBillingRepository{db: db}
	result, err := repo.Apply(context.Background(), newRetryTestCommand())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Applied, "replay of a committed charge must be a dedup hit")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingApply_RetriesAreBounded(t *testing.T) {
	withFastUsageBillingRetries(t)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	transient := &pq.Error{Code: "40001", Message: "could not serialize access"}
	for i := 0; i < 1+len(usageBillingApplyRetryBackoffs); i++ {
		expectClaimFails(mock, transient)
	}

	repo := &usageBillingRepository{db: db}
	result, err := repo.Apply(context.Background(), newRetryTestCommand())
	require.Nil(t, result)
	require.ErrorIs(t, err, transient)
	require.NoError(t, mock.ExpectationsWereMet(), "exactly 1 attempt + %d retries", len(usageBillingApplyRetryBackoffs))
}

func TestUsageBillingApply_DoesNotRetryNonTransientErrors(t *testing.T) {
	cases := map[string]error{
		"fingerprint conflict": service.ErrUsageBillingRequestConflict,
		"too many connections": &pq.Error{Code: "53300", Message: "too many connections"},
		"deadline exceeded":    context.DeadlineExceeded,
		"unique violation":     &pq.Error{Code: "23505", Message: "duplicate key"},
		"generic":              errors.New("boom"),
	}
	for name, failure := range cases {
		t.Run(name, func(t *testing.T) {
			withFastUsageBillingRetries(t)
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()

			expectClaimFails(mock, failure)

			repo := &usageBillingRepository{db: db}
			result, err := repo.Apply(context.Background(), newRetryTestCommand())
			require.Nil(t, result)
			require.ErrorIs(t, err, failure)
			require.NoError(t, mock.ExpectationsWereMet(), "non-transient errors must not be retried")
		})
	}
}

func TestUsageBillingApply_StopsRetryingWhenContextDone(t *testing.T) {
	prev := usageBillingApplyRetryBackoffs
	usageBillingApplyRetryBackoffs = []time.Duration{time.Hour, time.Hour}
	t.Cleanup(func() { usageBillingApplyRetryBackoffs = prev })

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	expectClaimFails(mock, &pq.Error{Code: "40P01", Message: "deadlock detected"})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	repo := &usageBillingRepository{db: db}
	result, err := repo.Apply(ctx, newRetryTestCommand())
	require.Nil(t, result)
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second, "backoff must honor the caller's deadline")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIsRetryableUsageBillingError(t *testing.T) {
	retryable := []error{
		driver.ErrBadConn,
		fmt.Errorf("wrapped: %w", driver.ErrBadConn),
		&pq.Error{Code: "40P01"},
		&pq.Error{Code: "40001"},
		&pq.Error{Code: "57P01"},
		&pq.Error{Code: "08000"},
		&pq.Error{Code: "08006"},
		fmt.Errorf("wrapped: %w", &pq.Error{Code: "08003"}),
	}
	for _, err := range retryable {
		require.True(t, isRetryableUsageBillingError(err), "%v should be retryable", err)
	}
	notRetryable := []error{
		nil,
		context.Canceled,
		context.DeadlineExceeded,
		sql.ErrNoRows,
		service.ErrUsageBillingRequestConflict,
		service.ErrUsageBillingRequestIDRequired,
		&pq.Error{Code: "53300"},
		&pq.Error{Code: "23505"},
		&pq.Error{Code: "57014"},
		errors.New("boom"),
	}
	for _, err := range notRetryable {
		require.False(t, isRetryableUsageBillingError(err), "%v should not be retryable", err)
	}
}

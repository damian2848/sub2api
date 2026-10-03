//go:build unit

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProbeRequestFactsRecorderIsNonbillingAtomicAndIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &channelMonitorV2Repository{db: db}
	started := time.Now().UTC()
	groupID := int64(4)
	successStatus := 200
	tokens := int64(5)
	rows := []service.ProbeRequestFact{
		{RunID: "run", LogicalRequestID: "logical", AttemptID: "attempt1", AttemptNumber: 1, Role: "generation", Platform: "openai", GroupID: &groupID, AccountID: 1, Protocol: "openai", RequestedModel: "model", UpstreamModel: "mapped", StartedAt: started, FinishedAt: started.Add(time.Second), DurationMs: 1000},
		{RunID: "run", LogicalRequestID: "logical", AttemptID: "attempt2", AttemptNumber: 2, IsFinal: true, Role: "generation", Platform: "openai", GroupID: &groupID, AccountID: 2, Protocol: "openai", RequestedModel: "model", UpstreamModel: "mapped", StartedAt: started.Add(time.Second), FinishedAt: started.Add(2 * time.Second), DurationMs: 1000, HTTPStatus: &successStatus, APISuccess: true, InputTokens: &tokens, UsageComplete: true},
	}
	mock.ExpectBegin()
	for range rows {
		mock.ExpectExec(`(?s)INSERT INTO probe_request_facts.*ON CONFLICT \(attempt_id\) DO NOTHING`).WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()
	require.NoError(t, repo.RecordProbeRequestFacts(context.Background(), rows))
	require.NoError(t, mock.ExpectationsWereMet())
	// A second identical save is permitted; PostgreSQL owns the attempt id guard.
	mock.ExpectBegin()
	for range rows {
		mock.ExpectExec(`(?s)INSERT INTO probe_request_facts.*ON CONFLICT \(attempt_id\) DO NOTHING`).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectCommit()
	require.NoError(t, repo.RecordProbeRequestFacts(context.Background(), rows))
	require.NoError(t, mock.ExpectationsWereMet())
	// No usage_logs insertion, balance mutation, ledger debit or credit replay.
}

func TestProbeRequestFactsRecorderRollsBackWholeLogicalRequest(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &channelMonitorV2Repository{db: db}
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO probe_request_facts`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO probe_request_facts`).WillReturnError(errors.New("database write failed"))
	mock.ExpectRollback()
	err = repo.RecordProbeRequestFacts(context.Background(), []service.ProbeRequestFact{{AttemptID: "first"}, {AttemptID: "second"}})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

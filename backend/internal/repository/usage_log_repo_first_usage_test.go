//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestUsageLogRepositoryGetAccountFirstUsageSince(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newUsageLogRepositoryWithSQL(nil, db)
	since := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	first := since.Add(42 * time.Minute)

	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs WHERE account_id = \$1 AND created_at >= \$2`).
		WithArgs(int64(9), since).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(first))
	got, err := repo.GetAccountFirstUsageSince(context.Background(), 9, since)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.True(t, got.Equal(first))

	mock.ExpectQuery(`SELECT MIN\(created_at\) FROM usage_logs`).
		WithArgs(int64(9), since).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(nil))
	got, err = repo.GetAccountFirstUsageSince(context.Background(), 9, since)
	require.NoError(t, err)
	require.Nil(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

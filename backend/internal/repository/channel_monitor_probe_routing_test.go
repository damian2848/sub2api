//go:build unit

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorAPIKeyBindingReadsCurrentGroup(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorRepository{db: db}
	for _, groupID := range []int64{3, 24} {
		mock.ExpectQuery(`SELECT id,group_id FROM api_keys WHERE key=\$1 AND deleted_at IS NULL`).
			WithArgs("synthetic-key").
			WillReturnRows(sqlmock.NewRows([]string{"id", "group_id"}).AddRow(11, groupID))
		binding, err := repo.GetMonitorAPIKeyBinding(context.Background(), "synthetic-key")
		require.NoError(t, err)
		require.Equal(t, int64(11), binding.APIKeyID)
		require.Equal(t, groupID, *binding.GroupID)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestChannelMonitorAPIKeyBindingAbsentAndUnbound(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorRepository{db: db}
	mock.ExpectQuery(`SELECT id,group_id FROM api_keys WHERE key=\$1 AND deleted_at IS NULL`).
		WithArgs("external-key").WillReturnRows(sqlmock.NewRows([]string{"id", "group_id"}))
	binding, err := repo.GetMonitorAPIKeyBinding(context.Background(), "external-key")
	require.NoError(t, err)
	require.Nil(t, binding)
	mock.ExpectQuery(`SELECT id,group_id FROM api_keys WHERE key=\$1 AND deleted_at IS NULL`).
		WithArgs("unbound-key").WillReturnRows(sqlmock.NewRows([]string{"id", "group_id"}).AddRow(11, nil))
	binding, err = repo.GetMonitorAPIKeyBinding(context.Background(), "unbound-key")
	require.NoError(t, err)
	require.Nil(t, binding.GroupID)
	mock.ExpectQuery(`SELECT id,group_id FROM api_keys WHERE key=\$1 AND deleted_at IS NULL`).
		WithArgs("db-error-key").WillReturnError(errors.New("unavailable"))
	_, err = repo.GetMonitorAPIKeyBinding(context.Background(), "db-error-key")
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

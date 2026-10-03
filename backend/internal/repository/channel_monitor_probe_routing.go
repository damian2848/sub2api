package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// GetMonitorAPIKeyBinding deliberately bypasses the auth cache: a key rebound
// after monitor creation must stop proving availability for its old group.
func (r *channelMonitorRepository) GetMonitorAPIKeyBinding(ctx context.Context, credential string) (*service.ChannelMonitorAPIKeyBinding, error) {
	binding := &service.ChannelMonitorAPIKeyBinding{}
	var groupID sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT id,group_id FROM api_keys WHERE key=$1 AND deleted_at IS NULL`, credential).
		Scan(&binding.APIKeyID, &groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if groupID.Valid {
		binding.GroupID = &groupID.Int64
	}
	return binding, nil
}

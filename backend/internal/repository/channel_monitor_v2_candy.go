package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func (r *channelMonitorV2Repository) ClaimCandyProbe(ctx context.Context, probe service.ChannelMonitorV2CandyProbe, key string, slot time.Time, version int) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `INSERT INTO channel_monitor_v2_candy_results(group_id,config_key,model,reasoning_effort,slot,requested_model)
	 SELECT $1,$2,$3,$4,$5,$3 FROM channel_monitor_v2_config WHERE id=1 AND enabled AND version=$6
 ON CONFLICT DO NOTHING RETURNING id`, probe.GroupID, key, probe.Model, probe.ReasoningEffort, slot, version).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (r *channelMonitorV2Repository) FinishCandyProbe(ctx context.Context, result service.ChannelMonitorV2CandyResult) error {
	var metering any
	if result.Usage != nil {
		data, err := json.Marshal(result.Usage)
		if err != nil {
			return err
		}
		metering = string(data)
	}
	attempts := result.Attempts
	if attempts == nil {
		attempts = []service.ChannelMonitorProbeAttempt{}
	}
	data, err := json.Marshal(attempts)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `UPDATE channel_monitor_v2_candy_results SET verdict=$2,latency_ms=$3,answer_preview=$4,reason=$5,finished_at=NOW(),
	 platform=$6,account_id=NULLIF($7,0),requested_model=COALESCE(NULLIF($8,''),model),upstream_model=$9,attempt_count=$10,metering=$11,probe_attempts=$12
	 WHERE id=$1 AND verdict='running'`, result.ID, result.Verdict, result.LatencyMs, result.AnswerPreview, result.Reason, result.Platform, result.AccountID, result.RequestedModel, result.UpstreamModel, result.AttemptCount, metering, string(data))
	return err
}

func (r *channelMonitorV2Repository) CandyHistory(ctx context.Context, configs map[int64]string, since time.Time) ([]service.ChannelMonitorV2CandyResult, error) {
	plural := make(map[int64][]string, len(configs))
	for group, key := range configs {
		plural[group] = []string{key}
	}
	return r.CandyHistoryMany(ctx, plural, since)
}

func (r *channelMonitorV2Repository) CandyHistoryMany(ctx context.Context, configs map[int64][]string, since time.Time) ([]service.ChannelMonitorV2CandyResult, error) {
	out := []service.ChannelMonitorV2CandyResult{}
	if len(configs) == 0 {
		return out, nil
	}
	ids := make([]int64, 0, len(configs))
	keys := make([]string, 0, len(configs))
	for id, configKeys := range configs {
		for _, key := range configKeys {
			ids = append(ids, id)
			keys = append(keys, key)
		}
	}
	// Rank each active configuration independently so another model cannot displace its history.
	rows, err := r.db.QueryContext(ctx, `SELECT id,group_id,config_key,checked_at,verdict,latency_ms,answer_preview,reason,
	 source,platform,account_id,COALESCE(NULLIF(requested_model,''),model),upstream_model,attempt_count,metering,probe_attempts
	 FROM (SELECT r.*,ROW_NUMBER() OVER (PARTITION BY r.group_id,r.config_key ORDER BY r.checked_at DESC,r.id DESC) AS rn
 FROM channel_monitor_v2_candy_results r
 JOIN unnest($1::bigint[], $2::text[]) AS wanted(group_id,config_key)
 ON r.group_id=wanted.group_id AND r.config_key=wanted.config_key
 WHERE r.checked_at >= $3 AND r.verdict<>'running') recent
 WHERE rn<=$4 ORDER BY checked_at,id`, pq.Array(ids), pq.Array(keys), since, service.ChannelMonitorV2CandyHistoryLimit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item service.ChannelMonitorV2CandyResult
		var account sql.NullInt64
		var metering, attempts []byte
		if err := rows.Scan(&item.ID, &item.GroupID, &item.ConfigKey, &item.CheckedAt, &item.Verdict, &item.LatencyMs, &item.AnswerPreview, &item.Reason, &item.Source, &item.Platform, &account, &item.RequestedModel, &item.UpstreamModel, &item.AttemptCount, &metering, &attempts); err != nil {
			return nil, err
		}
		item.AccountID = account.Int64
		if len(metering) > 0 {
			if err := json.Unmarshal(metering, &item.Usage); err != nil {
				return nil, err
			}
		}
		if len(attempts) > 0 {
			if err := json.Unmarshal(attempts, &item.Attempts); err != nil {
				return nil, err
			}
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *channelMonitorV2Repository) PruneCandyHistory(ctx context.Context, now time.Time) error {
	if err := r.pruneMonitorObservationHistory(ctx, now); err != nil {
		return err
	}
	// A crashed worker's claim must never block a group indefinitely. The lease
	// is longer than the 90-second probe timeout; old workers cannot overwrite it.
	if _, err := r.db.ExecContext(ctx, `UPDATE channel_monitor_v2_candy_results SET verdict='error',reason='interrupted',finished_at=NOW() WHERE verdict='running' AND checked_at<$1`, now.Add(-3*time.Minute)); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM channel_monitor_v2_candy_results WHERE id IN (SELECT id FROM channel_monitor_v2_candy_results WHERE checked_at<$1 LIMIT 10000)`, now.Add(-service.ChannelMonitorV2CandyRetention))
	return err
}

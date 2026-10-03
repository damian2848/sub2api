package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.ProbeRequestFactRecorder = (*channelMonitorV2Repository)(nil)

func (r *channelMonitorV2Repository) RecordProbeRequestFacts(ctx context.Context, facts []service.ProbeRequestFact) error {
	if len(facts) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, f := range facts {
		_, err := tx.ExecContext(ctx, `INSERT INTO probe_request_facts
(run_id,logical_request_id,attempt_id,attempt_number,is_final,role,platform,group_id,account_id,protocol,requested_model,upstream_model,
 started_at,finished_at,duration_ms,first_token_ms,http_status,api_success,error_kind,
 input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,cache_creation_5m_tokens,cache_creation_1h_tokens,reasoning_tokens,
 usage_complete,upstream_cost_usd,cost_complete)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29)
ON CONFLICT (attempt_id) DO NOTHING`,
			f.RunID, f.LogicalRequestID, f.AttemptID, f.AttemptNumber, f.IsFinal, f.Role, f.Platform, f.GroupID, f.AccountID, f.Protocol, f.RequestedModel, f.UpstreamModel,
			f.StartedAt, f.FinishedAt, f.DurationMs, f.FirstTokenMs, f.HTTPStatus, f.APISuccess, f.ErrorKind,
			f.InputTokens, f.OutputTokens, f.CacheReadTokens, f.CacheCreationTokens, f.CacheCreation5mTokens, f.CacheCreation1hTokens, f.ReasoningTokens,
			f.UsageComplete, f.UpstreamCostUSD, f.CostComplete)
		if err != nil {
			return fmt.Errorf("insert probe request fact: %w", err)
		}
	}
	return tx.Commit()
}

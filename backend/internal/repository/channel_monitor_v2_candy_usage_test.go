package repository

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2CandyWritesRouteAndConsumptionTogether(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewChannelMonitorV2Repository(db).(*channelMonitorV2Repository)
	cost := 0.03
	usage := &service.ChannelMonitorProbeUsage{Source: "probe", RequestCount: 2, InputTokens: 20, OutputTokens: 10, CacheReadTokens: 60, ReasoningTokens: 5, CostUSD: &cost, CostIncomplete: true}
	attempts := []service.ChannelMonitorProbeAttempt{{AccountID: 77, Platform: service.PlatformOpenAI, RequestedModel: "public", UpstreamModel: "mapped", Usage: usage}}
	usageJSON, err := json.Marshal(usage)
	require.NoError(t, err)
	attemptsJSON, err := json.Marshal(attempts)
	require.NoError(t, err)
	mock.ExpectExec(`(?s)UPDATE channel_monitor_v2_candy_results SET .*metering=\$11,probe_attempts=\$12.*WHERE id=\$1 AND verdict='running'`).
		WithArgs(int64(8), "incorrect", int64(120), "22", "", service.PlatformOpenAI, int64(77), "public", "mapped", 2, string(usageJSON), string(attemptsJSON)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.FinishCandyProbe(context.Background(), service.ChannelMonitorV2CandyResult{
		ID: 8, Verdict: "incorrect", LatencyMs: 120, AnswerPreview: "22", Platform: service.PlatformOpenAI, AccountID: 77,
		RequestedModel: "public", UpstreamModel: "mapped", AttemptCount: 2, Usage: usage, Attempts: attempts,
	}))
	require.NoError(t, mock.ExpectationsWereMet())
	invalidCost := math.NaN()
	require.Error(t, repo.FinishCandyProbe(context.Background(), service.ChannelMonitorV2CandyResult{ID: 8, Usage: &service.ChannelMonitorProbeUsage{CostUSD: &invalidCost}}))
	require.NoError(t, mock.ExpectationsWereMet(), "invalid metering must fail before any database mutation")
}

func TestChannelMonitorV2CandyReadsIndependentConfigHistoryAndUnknownUsage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewChannelMonitorV2Repository(db).(*channelMonitorV2Repository)
	now := time.Now()
	cost := 0.01
	usage := &service.ChannelMonitorProbeUsage{Source: "probe", RequestCount: 1, InputTokens: 12, OutputTokens: 2, CostUSD: &cost}
	usageJSON, err := json.Marshal(usage)
	require.NoError(t, err)
	rows := sqlmock.NewRows([]string{"id", "group_id", "config_key", "checked_at", "verdict", "latency_ms", "answer_preview", "reason", "source", "platform", "account_id", "requested_model", "upstream_model", "attempt_count", "metering", "probe_attempts"}).
		AddRow(1, 4, "first-model", now, "correct", 10, "21", "", "probe", service.PlatformOpenAI, 77, "first-model", "mapped", 1, usageJSON, []byte("[]")).
		AddRow(2, 4, "second-model", now, "error", 12, "", "interrupted", "probe", "", nil, "second-model", "", 0, nil, []byte("[]"))
	mock.ExpectQuery(`(?s)SELECT .*PARTITION BY r.group_id,r.config_key.*unnest\(\$1::bigint\[\], \$2::text\[\]\).*verdict<>'running'.*WHERE rn<=\$4`).
		WithArgs(pq.Array([]int64{4, 4}), pq.Array([]string{"first-model", "second-model"}), now.Add(-service.ChannelMonitorV2CandyRetention), service.ChannelMonitorV2CandyHistoryLimit).
		WillReturnRows(rows)
	items, err := repo.CandyHistoryMany(context.Background(), map[int64][]string{4: {"first-model", "second-model"}}, now.Add(-service.ChannelMonitorV2CandyRetention))
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, usage, items[0].Usage)
	require.Equal(t, int64(77), items[0].AccountID)
	require.Equal(t, "mapped", items[0].UpstreamModel)
	require.Equal(t, "probe", items[1].Source)
	require.Nil(t, items[1].Usage, "legacy history remains unmetered")
	require.Zero(t, items[1].AccountID)
	require.NoError(t, mock.ExpectationsWereMet())
}

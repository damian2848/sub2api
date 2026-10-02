//go:build integration

package repository

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorObservationsFullWindowAndMovedGroup(t *testing.T) {
	ctx := context.Background()
	oldGroup := mustCreateGroup(t, integrationEntClient, &service.Group{Name: "observation-old", Platform: service.PlatformOpenAI})
	newGroup := mustCreateGroup(t, integrationEntClient, &service.Group{Name: "observation-new", Platform: service.PlatformOpenAI})
	t.Cleanup(func() {
		_ = integrationEntClient.Group.DeleteOneID(oldGroup.ID).Exec(ctx)
		_ = integrationEntClient.Group.DeleteOneID(newGroup.ID).Exec(ctx)
	})
	monitors := NewChannelMonitorRepository(integrationEntClient, integrationDB)
	monitor := &service.ChannelMonitor{Name: "observation-target", Provider: service.PlatformOpenAI, Endpoint: "https://example.com", APIKey: "encrypted", PrimaryModel: "primary", ExtraModels: []string{"secondary", "never-run"}, GroupID: &newGroup.ID, GroupName: "legacy label", Enabled: true, IntervalSeconds: 60}
	require.NoError(t, monitors.Create(ctx, monitor))
	t.Cleanup(func() { _ = monitors.Delete(ctx, monitor.ID) })
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := &service.ChannelMonitorObservationScope{Platform: service.PlatformOpenAI, GroupIDs: []int64{oldGroup.ID}, GroupNames: map[int64]string{oldGroup.ID: "original group name"}, Name: monitor.Name, CheckMode: service.MonitorCheckModeProbe}
	rows := make([]*service.ChannelMonitorHistoryRow, 0, 130)
	for i := 0; i < 130; i++ {
		model := "primary"
		if i >= 125 {
			model = "secondary"
		}
		rows = append(rows, &service.ChannelMonitorHistoryRow{MonitorID: monitor.ID, Model: model, Status: service.MonitorStatusOperational, CheckedAt: now.Add(-time.Duration(i+1) * time.Minute), Scope: scope, Usage: &service.ChannelMonitorProbeUsage{Source: "probe", RequestCount: 1, InputTokens: 10, OutputTokens: 5, UsageIncomplete: false, CostIncomplete: true}})
	}
	require.NoError(t, monitors.InsertHistoryBatch(ctx, rows))
	repo := NewChannelMonitorV2Repository(integrationDB).(*channelMonitorV2Repository)
	filter := service.ChannelMonitorV2Filter{Start: now.Add(-24 * time.Hour), End: now.Add(time.Minute), GroupIDs: []int64{oldGroup.ID}}
	observations, err := repo.ListMonitorObservations(ctx, filter)
	require.NoError(t, err)
	require.Len(t, observations, 2)
	byModel := map[string]service.ChannelMonitorObservation{}
	for _, item := range observations {
		byModel[item.Model] = item
		require.Equal(t, []int64{oldGroup.ID}, item.Scope.GroupIDs)
		require.Equal(t, "original group name", item.Scope.GroupNames[oldGroup.ID])
	}
	require.NotEqual(t, byModel["primary"].ID, byModel["secondary"].ID)
	require.Equal(t, int64(125), byModel["primary"].SampleCount)
	require.Equal(t, int64(125), byModel["primary"].PassedCount)
	require.Len(t, byModel["primary"].History, 100)
	require.Equal(t, int64(1250), byModel["primary"].Usage.InputTokens)
	require.Equal(t, int64(125), byModel["primary"].Usage.RequestCount)
	filter.GroupIDs = []int64{newGroup.ID}
	current, err := repo.ListMonitorObservations(ctx, filter)
	require.NoError(t, err)
	require.Len(t, current, 3, "current targets remain visible before their first sample in the new group")
	for _, item := range current {
		require.Zero(t, item.SampleCount)
		require.Equal(t, "unknown", item.Verdict)
	}
	targets, err := repo.ListMonitorObservationTargets(ctx, filter)
	require.NoError(t, err)
	foundHistoricalGroup := false
	for _, item := range targets {
		if item.Model == "primary" && item.GroupID != nil && *item.GroupID == oldGroup.ID {
			foundHistoricalGroup = true
		}
	}
	require.True(t, foundHistoricalGroup, "historical scope stays selectable after a target moves")
	filter.GroupIDs = nil
	filter.RestrictGroups = true
	filter.AllowedGroupIDs = []int64{oldGroup.ID}
	allowed, err := repo.ListMonitorObservations(ctx, filter)
	require.NoError(t, err)
	require.Len(t, allowed, 2)
}

func TestChannelMonitorQualityArchiveSurvivesPruningAndPlanDeletion(t *testing.T) {
	ctx := context.Background()
	group := mustCreateGroup(t, integrationEntClient, &service.Group{Name: "quality-observation", Platform: service.PlatformOpenAI})
	account := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "quality-observation", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture"}})
	t.Cleanup(func() {
		_ = integrationEntClient.Account.DeleteOneID(account.ID).Exec(ctx)
		_ = integrationEntClient.Group.DeleteOneID(group.ID).Exec(ctx)
	})
	now := time.Now().UTC().Truncate(time.Microsecond)
	plans := NewScheduledTestPlanRepository(integrationDB)
	results := NewScheduledTestResultRepository(integrationDB)
	config := &service.PelicanTestConfig{Quality: &service.QualityPolicy{}, ModelID: "quality-model"}
	plan, err := plans.Create(ctx, &service.ScheduledTestPlan{AccountID: account.ID, ModelID: "quality-model", CronExpression: "*/5 * * * *", Enabled: true, MaxResults: 1, PelicanConfig: config})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM channel_monitor_quality_observations WHERE plan_id=$1`, plan.ID)
	})
	scope := &service.ChannelMonitorObservationScope{Platform: service.PlatformOpenAI, AccountID: &account.ID, Name: account.Name, GroupIDs: []int64{group.ID}, GroupNames: map[int64]string{group.ID: group.Name}}
	for i := 0; i < 3; i++ {
		var usage *service.ChannelMonitorProbeUsage
		if i < 2 {
			cost := 0.1
			usage = &service.ChannelMonitorProbeUsage{Source: "probe", RequestCount: 2, InputTokens: 10, OutputTokens: 20, CostUSD: &cost}
		}
		_, err := results.Create(ctx, &service.ScheduledTestResult{PlanID: plan.ID, Status: "success", StartedAt: now.Add(-time.Duration(i) * time.Minute), FinishedAt: now, PelicanConfig: config, QualityJudgment: &service.QualityJudgment{Verdict: "correct"}, Usage: usage, ObservationScope: scope})
		require.NoError(t, err)
	}
	require.NoError(t, results.PruneOldResults(ctx, plan.ID, 1))
	saved, err := results.ListByPlanID(ctx, plan.ID, 10)
	require.NoError(t, err)
	require.Len(t, saved, 1)
	repo := NewChannelMonitorV2Repository(integrationDB).(*channelMonitorV2Repository)
	filter := service.ChannelMonitorV2Filter{Start: now.Add(-time.Hour), End: now.Add(time.Minute), GroupIDs: []int64{group.ID}}
	observations, err := repo.ListMonitorObservations(ctx, filter)
	require.NoError(t, err)
	require.Len(t, observations, 1)
	require.Equal(t, int64(3), observations[0].SampleCount, "archives and source rows must not double count")
	require.Equal(t, int64(3), observations[0].PassedCount)
	require.Equal(t, int64(4), observations[0].Usage.RequestCount)
	require.Equal(t, int64(20), observations[0].Usage.InputTokens)
	require.True(t, observations[0].Usage.UsageIncomplete, "legacy usage remains unknown")
	require.True(t, observations[0].Usage.CostIncomplete)
	require.NotNil(t, observations[0].Usage.CostUSD)
	require.InDelta(t, 0.2, *observations[0].Usage.CostUSD, 0.000001)
	require.NoError(t, plans.Delete(ctx, plan.ID))
	observations, err = repo.ListMonitorObservations(ctx, filter)
	require.NoError(t, err)
	require.Len(t, observations, 1)
	require.Equal(t, int64(3), observations[0].SampleCount)
	require.False(t, observations[0].Enabled)
	require.NoError(t, repo.RecordStateProbe(ctx, &service.OpenAICodexStateProbeResult{Model: "state-model", Verdict: "degraded", StartedAt: now, LatencyMs: 50, Scope: scope, Usage: &service.ChannelMonitorProbeUsage{Source: "probe", RequestCount: 1}}))
	observations, err = repo.ListMonitorObservations(ctx, filter)
	require.NoError(t, err)
	require.Len(t, observations, 2)
	for _, item := range observations {
		if item.Type == "state_probe" {
			require.Equal(t, int64(1), item.FailedCount)
			require.Equal(t, int64(1), item.Usage.RequestCount)
		}
	}
	_, err = integrationDB.ExecContext(ctx, `UPDATE channel_monitor_quality_observations SET checked_at=$2 WHERE plan_id=$1`, plan.ID, now.Add(-31*24*time.Hour))
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE channel_monitor_state_probe_results SET checked_at=$2 WHERE observation_scope->>'account_id'=$1`, strconv.FormatInt(account.ID, 10), now.Add(-31*24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, repo.pruneMonitorObservationHistory(ctx, now))
	observations, err = repo.ListMonitorObservations(ctx, filter)
	require.NoError(t, err)
	require.Empty(t, observations)
}

func TestChannelMonitorBusinessAggregationExcludesProbes(t *testing.T) {
	ctx := context.Background()
	group := mustCreateGroup(t, integrationEntClient, &service.Group{Name: "monitor-business-source", Platform: service.PlatformOpenAI})
	user := mustCreateUser(t, integrationEntClient, &service.User{})
	key := mustCreateApiKey(t, integrationEntClient, &service.APIKey{UserID: user.ID, Key: "monitor-source-" + strconv.FormatInt(time.Now().UnixNano(), 10), Name: "monitor-source"})
	account := mustCreateAccount(t, integrationEntClient, &service.Account{Name: "monitor-source", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_error_logs WHERE group_id=$1`, group.ID)
		for _, table := range []string{"channel_monitor_v2_latency_histograms_rollup", "channel_monitor_v2_error_metrics_rollup", "channel_monitor_v2_user_metrics_rollup", "channel_monitor_v2_metrics_rollup", "channel_monitor_v2_latency_histograms_1m", "channel_monitor_v2_error_metrics_1m", "channel_monitor_v2_user_metrics_1m", "channel_monitor_v2_metrics_1m"} {
			_, _ = integrationDB.ExecContext(ctx, "DELETE FROM "+table+" WHERE group_id=$1", group.ID)
		}
		_ = integrationEntClient.User.DeleteOneID(user.ID).Exec(ctx)
		_ = integrationEntClient.Account.DeleteOneID(account.ID).Exec(ctx)
		_ = integrationEntClient.Group.DeleteOneID(group.ID).Exec(ctx)
	})
	now := time.Now().UTC().Truncate(time.Minute)
	firstToken, duration := 12, 24
	usageRepo := NewUsageLogRepository(integrationEntClient, integrationDB)
	for _, source := range []string{service.RequestSourceBusiness, service.RequestSourceProbe} {
		_, err := usageRepo.Create(ctx, &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, GroupID: &group.ID, RequestID: source + "-usage", Model: "source-model", InputTokens: 10, OutputTokens: 5, FirstTokenMs: &firstToken, DurationMs: &duration, TotalCost: 0.2, ActualCost: 0.2, Source: source, CreatedAt: now})
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `INSERT INTO ops_error_logs(request_id,user_id,group_id,account_id,platform,model,error_phase,error_type,status_code,source,created_at) VALUES($1,$2,$3,$4,'openai','source-model','upstream','internal',500,$5,$6)`, source+"-error", user.ID, group.ID, account.ID, source, now)
		require.NoError(t, err)
	}
	repo := NewChannelMonitorV2Repository(integrationDB).(*channelMonitorV2Repository)
	require.NoError(t, repo.RecomputeRange(ctx, now, now.Add(time.Minute)))
	var success, failed, input, output int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COALESCE(sum(success_requests),0),COALESCE(sum(error_requests),0),COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0) FROM channel_monitor_v2_metrics_1m WHERE group_id=$1`, group.ID).Scan(&success, &failed, &input, &output))
	require.Equal(t, int64(1), success)
	require.Equal(t, int64(1), failed)
	require.Equal(t, int64(10), input)
	require.Equal(t, int64(5), output)
	var latencySamples int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COALESCE(sum(sample_count),0) FROM channel_monitor_v2_latency_histograms_1m WHERE group_id=$1 AND user_id=0 AND metric='ttft'`, group.ID).Scan(&latencySamples))
	require.Equal(t, int64(1), latencySamples, "probe latency must not skew business latency")
	var persistedCost float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT sum(actual_cost) FROM usage_logs WHERE group_id=$1`, group.ID).Scan(&persistedCost))
	require.InDelta(t, 0.4, persistedCost, 0.000001, "source separation must not alter persisted billing amounts")
}

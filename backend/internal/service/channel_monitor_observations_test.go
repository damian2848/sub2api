//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type monitorObservationsRepoFake struct {
	channelMonitorV2RepoStub
	rows []ChannelMonitorObservation
}

func (r *monitorObservationsRepoFake) ListMonitorObservations(context.Context, ChannelMonitorV2Filter) ([]ChannelMonitorObservation, error) {
	return append([]ChannelMonitorObservation{}, r.rows...), nil
}

func TestChannelMonitorObservationsScopePrivacyAndTotals(t *testing.T) {
	cost := 0.4
	now := time.Now().UTC()
	row := ChannelMonitorObservation{ID: "account-secret", Type: "state_probe", Name: "private account", Platform: PlatformOpenAI, Model: "gpt-test", SampleCount: 12, PassedCount: 8, FailedCount: 3, InconclusiveCount: 1, CheckedAt: &now, Scope: ChannelMonitorObservationScope{GroupIDs: []int64{1, 2}, GroupNames: map[int64]string{1: "Allowed", 2: "Other"}}, Usage: ChannelMonitorProbeUsage{Source: "probe", RequestCount: 24, InputTokens: 100, OutputTokens: 30, CacheReadTokens: 20, ReasoningTokens: 15, CostUSD: &cost}, History: []ChannelMonitorObservationPoint{{CheckedAt: now, Verdict: "degraded", Message: "private upstream failure"}}}
	repo := &monitorObservationsRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: ChannelMonitorV2Config{Platforms: []ChannelMonitorV2PlatformConfig{{Platform: PlatformOpenAI, Enabled: true}}}}, rows: []ChannelMonitorObservation{row}}
	svc := NewChannelMonitorV2Service(repo)
	filter := ChannelMonitorV2Filter{RestrictGroups: true, AllowedGroupIDs: []int64{1}}
	public, err := svc.Observations(context.Background(), filter, ChannelMonitorV2GroupByPlatformGroupModel, false)
	require.NoError(t, err)
	require.Len(t, public.Items, 1)
	require.Equal(t, int64(1), *public.Items[0].GroupID)
	require.NotEqual(t, row.ID, public.Items[0].ID)
	require.Equal(t, "state_probe", public.Items[0].Name)
	require.Empty(t, public.Items[0].History[0].Message)
	require.Zero(t, public.Items[0].Usage.InputTokens)
	require.Nil(t, public.Items[0].Usage.CostUSD)
	admin, err := svc.Observations(context.Background(), ChannelMonitorV2Filter{}, ChannelMonitorV2GroupByPlatform, true)
	require.NoError(t, err)
	require.Len(t, admin.Items, 1, "multiple memberships must not multiply platform totals")
	require.Equal(t, int64(12), admin.Summary.ProbeCount)
	require.Equal(t, int64(12), admin.Items[0].SampleCount)
	require.Equal(t, int64(150), admin.TotalTokens, "reasoning is a subset of output")
	require.Equal(t, int64(100), admin.Items[0].Usage.InputTokens)
	require.Equal(t, "private upstream failure", admin.Items[0].History[0].Message)
}

func TestChannelMonitorObservationsCountEveryModelOnce(t *testing.T) {
	repo := &monitorObservationsRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: ChannelMonitorV2Config{Platforms: []ChannelMonitorV2PlatformConfig{{Platform: PlatformOpenAI, Enabled: true}}}}, rows: []ChannelMonitorObservation{
		{ID: "target", Type: "connectivity", Platform: PlatformOpenAI, Model: "first", SampleCount: 2, Usage: ChannelMonitorProbeUsage{InputTokens: 10}, Scope: ChannelMonitorObservationScope{GroupIDs: []int64{1, 2}}},
		{ID: "target", Type: "connectivity", Platform: PlatformOpenAI, Model: "second", SampleCount: 3, Usage: ChannelMonitorProbeUsage{InputTokens: 20}, Scope: ChannelMonitorObservationScope{GroupIDs: []int64{1, 2}}},
	}}
	result, err := NewChannelMonitorV2Service(repo).Observations(context.Background(), ChannelMonitorV2Filter{}, ChannelMonitorV2GroupByPlatform, true)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	require.Equal(t, int64(5), result.Summary.ProbeCount)
	require.Equal(t, int64(5), result.Items[0].SampleCount)
	require.Equal(t, int64(30), result.TotalTokens)
}

func TestChannelMonitorObservationsOtherModelFilter(t *testing.T) {
	cfg := &ChannelMonitorV2Config{Platforms: []ChannelMonitorV2PlatformConfig{{Platform: PlatformOpenAI, Enabled: true, Models: []string{"named"}}, {Platform: PlatformAnthropic, Enabled: true}}}
	filter := ChannelMonitorV2Filter{Models: []string{ChannelMonitorV2OtherModel}}
	require.True(t, observationModelAllowed(PlatformOpenAI, "probe-only", filter, cfg))
	require.False(t, observationModelAllowed(PlatformOpenAI, "named", filter, cfg))
	require.False(t, observationModelAllowed(PlatformAnthropic, "claude-test", filter, cfg))
	filter.Models = []string{"probe-only"}
	require.True(t, observationModelAllowed(PlatformOpenAI, "probe-only", filter, cfg))
}

func TestChannelMonitorObservationGroupCostsDoNotSharePointers(t *testing.T) {
	firstCost, secondCost := 0.1, 0.2
	repo := &monitorObservationsRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: ChannelMonitorV2Config{Platforms: []ChannelMonitorV2PlatformConfig{{Platform: PlatformOpenAI, Enabled: true}}}}, rows: []ChannelMonitorObservation{
		{ID: "first", Type: "quality", Platform: PlatformOpenAI, Model: "model", SampleCount: 1, Usage: ChannelMonitorProbeUsage{CostUSD: &firstCost}, Scope: ChannelMonitorObservationScope{GroupIDs: []int64{1, 2}}},
		{ID: "second", Type: "quality", Platform: PlatformOpenAI, Model: "model", SampleCount: 1, Usage: ChannelMonitorProbeUsage{CostUSD: &secondCost}, Scope: ChannelMonitorObservationScope{GroupIDs: []int64{1, 2}}},
	}}
	result, err := NewChannelMonitorV2Service(repo).Observations(context.Background(), ChannelMonitorV2Filter{}, ChannelMonitorV2GroupByPlatformGroup, true)
	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	require.InDelta(t, 0.3, *result.Summary.Usage.CostUSD, 0.000001)
	for _, item := range result.Items {
		require.InDelta(t, 0.3, *item.Usage.CostUSD, 0.000001)
	}
	require.Equal(t, 0.1, firstCost)
	require.Equal(t, 0.2, secondCost)
}

func TestChannelMonitorObservationsRejectUnboundAndDeniedGroups(t *testing.T) {
	repo := &monitorObservationsRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: ChannelMonitorV2Config{Platforms: []ChannelMonitorV2PlatformConfig{{Platform: PlatformOpenAI, Enabled: true}}, GroupIDs: []int64{2}}}, rows: []ChannelMonitorObservation{{ID: "unbound", Platform: PlatformOpenAI}, {ID: "denied", Platform: PlatformOpenAI, Scope: ChannelMonitorObservationScope{GroupIDs: []int64{2}}}}}
	result, err := NewChannelMonitorV2Service(repo).Observations(context.Background(), ChannelMonitorV2Filter{RestrictGroups: true, AllowedGroupIDs: []int64{1}}, ChannelMonitorV2GroupByPlatformGroupModel, false)
	require.NoError(t, err)
	require.Empty(t, result.Items)
	require.Zero(t, result.Summary.ProbeCount)
}

func TestChannelMonitorObservationsCollectionDisabledKeepsHistory(t *testing.T) {
	repo := &monitorObservationsRepoFake{channelMonitorV2RepoStub: channelMonitorV2RepoStub{config: ChannelMonitorV2Config{Enabled: false, Platforms: []ChannelMonitorV2PlatformConfig{{Platform: PlatformOpenAI, Enabled: true}}}}, rows: []ChannelMonitorObservation{{ID: "target", Type: "connectivity", Platform: PlatformOpenAI, Enabled: true, SampleCount: 1}}}
	svc := NewChannelMonitorV2Service(repo)
	svc.SetRuntimeReader(channelMonitorRuntimeStub{rt: ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV2}})
	result, err := svc.Observations(context.Background(), ChannelMonitorV2Filter{}, ChannelMonitorV2GroupByPlatformGroupModel, true)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	require.False(t, result.Items[0].Enabled)
	require.Equal(t, int64(1), result.Summary.ProbeCount)
}

func TestChannelMonitorActiveDimensionsRespectGroupPermissions(t *testing.T) {
	dims := &ChannelMonitorV2Dimensions{}
	rows := []ChannelMonitorObservation{{Platform: PlatformOpenAI, Model: "probe-only", Scope: ChannelMonitorObservationScope{GroupIDs: []int64{7, 8}, GroupNames: map[int64]string{7: "Visible", 8: "Private"}}}, {Platform: PlatformOpenAI, Model: "unbound"}}
	mergeMonitorObservationDimensions(dims, rows, ChannelMonitorV2Filter{RestrictGroups: true, AllowedGroupIDs: []int64{7}}, &ChannelMonitorV2Config{Platforms: []ChannelMonitorV2PlatformConfig{{Platform: PlatformOpenAI, Enabled: true}}})
	require.Len(t, dims.Models, 1)
	require.Equal(t, "probe-only", dims.Models[0].Value)
	require.Len(t, dims.Groups, 1)
	require.Equal(t, int64(7), dims.Groups[0].ID)
}

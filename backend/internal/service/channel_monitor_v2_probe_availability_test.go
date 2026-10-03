package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type availabilityProbeRepo struct {
	channelMonitorV2RepoStub
	rows   []ChannelMonitorObservation
	models []ChannelMonitorV2ModelRow
	err    error
}

func (r *availabilityProbeRepo) ListMonitorObservations(context.Context, ChannelMonitorV2Filter) ([]ChannelMonitorObservation, error) {
	return r.rows, r.err
}

func (r *availabilityProbeRepo) GetModels(context.Context, ChannelMonitorV2Filter, ChannelMonitorV2Config, bool) (*ChannelMonitorV2List[ChannelMonitorV2ModelRow], error) {
	return &ChannelMonitorV2List[ChannelMonitorV2ModelRow]{Items: append([]ChannelMonitorV2ModelRow{}, r.models...)}, nil
}

func (r *availabilityProbeRepo) GetMatrix(_ context.Context, _ ChannelMonitorV2Filter, _ ChannelMonitorV2Config, by ChannelMonitorV2GroupBy, _ bool) (*ChannelMonitorV2Matrix, error) {
	return &ChannelMonitorV2Matrix{GroupBy: by, Items: append([]ChannelMonitorV2MatrixRow{}, r.matrix.Items...)}, nil
}

type availabilityProbeRuntime struct{ runtime ChannelMonitorRuntime }

func (r availabilityProbeRuntime) GetChannelMonitorRuntime(context.Context) ChannelMonitorRuntime {
	return r.runtime
}

func newAvailabilityProbeFixture() (*ChannelMonitorV2Service, *availabilityProbeRepo, ChannelMonitorV2Filter) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	checked := now.Add(-time.Minute)
	cfg := ChannelMonitorV2Config{Enabled: true, Platforms: []ChannelMonitorV2PlatformConfig{{Platform: PlatformOpenAI, Enabled: true}}}
	repo := &availabilityProbeRepo{
		channelMonitorV2RepoStub: channelMonitorV2RepoStub{
			config: cfg,
			snap:   &ChannelMonitorV2Snapshot{Config: cfg, Health: ChannelMonitorV2HealthFor(ChannelMonitorV2Metric{})},
			matrix: &ChannelMonitorV2Matrix{},
		},
		rows: []ChannelMonitorObservation{{
			ID: "probe-1", Type: "candy", Platform: PlatformOpenAI, Model: "probe-only", Enabled: true, IntervalSeconds: 1800,
			CheckedAt: &checked, SampleCount: 1, PassedCount: 1,
			Scope: ChannelMonitorObservationScope{GroupIDs: []int64{7, 8}, GroupNames: map[int64]string{7: "Visible", 8: "Other"}},
			Usage: ChannelMonitorProbeUsage{RequestCount: 1, InputTokens: 100, OutputTokens: 20},
		}},
	}
	svc := NewChannelMonitorV2Service(repo)
	svc.now = func() time.Time { return now }
	svc.SetRuntimeReader(availabilityProbeRuntime{ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeHybrid}})
	return svc, repo, ChannelMonitorV2Filter{Start: now.Add(-90 * time.Minute), End: now.Add(time.Minute), Bucket: 5 * time.Minute}
}

func TestChannelMonitorV2ObservationVerdictsNeverManufactureRequestAvailability(t *testing.T) {
	for _, tt := range []struct {
		name                  string
		passed, failed, other int64
		availability          float64
		health                string
	}{
		{"success", 1, 0, 0, 1, "healthy"},
		{"failure", 0, 1, 0, 0, "critical"},
		{"mixed", 9, 1, 0, 0.9, "warning"},
		{"critical threshold", 4, 1, 0, 0.8, "critical"},
		{"exclude inconclusive", 1, 1, 98, 0.5, "critical"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo, filter := newAvailabilityProbeFixture()
			probe := &repo.rows[0]
			probe.SampleCount = tt.passed + tt.failed + tt.other
			probe.PassedCount, probe.FailedCount, probe.InconclusiveCount = tt.passed, tt.failed, tt.other
			snapshot, err := svc.Snapshot(context.Background(), filter, true)
			require.NoError(t, err)
			require.Equal(t, "unknown", snapshot.Metrics.AvailabilitySource)
			require.Nil(t, snapshot.Metrics.ProbeAvailability)
			require.Equal(t, probe.SampleCount, snapshot.Metrics.ProbeSampleCount, "multi-group probes count once in the snapshot")
			require.Equal(t, "unknown", snapshot.Health.Overall)
			require.Equal(t, "unknown", snapshot.Health.TTFT)
			require.Equal(t, "unknown", snapshot.Health.Cache)
			require.Equal(t, DefaultChannelMonitorV2HealthThresholds().MinimumSample, snapshot.Health.MinimumSample)
			require.Equal(t, probe.CheckedAt, snapshot.Metrics.ProbeCheckedAt)
			require.False(t, snapshot.Metrics.HasSamples)
			require.Zero(t, snapshot.Metrics.RequestCount)
			require.Zero(t, snapshot.Metrics.ErrorRequests)
			require.Zero(t, snapshot.Metrics.ErrorRate)
			require.Zero(t, snapshot.Metrics.InputTokens)
			require.Zero(t, snapshot.Metrics.OutputTokens)
			require.Zero(t, snapshot.Metrics.TokenCount)
			require.Zero(t, snapshot.Metrics.RPM)
			require.Zero(t, snapshot.Metrics.TPM)
			require.Nil(t, snapshot.Metrics.TTFT.P50Ms)
			require.Empty(t, snapshot.Trend, "do not invent historical business buckets")
		})
	}
}

func TestChannelMonitorV2ProbeAvailabilityRequiresActiveFreshResults(t *testing.T) {
	for _, name := range []string{"no probes", "never executed", "missing time", "inconclusive", "stale", "disabled", "manual", "runtime off", "missing runtime", "candy collection off", "outside window", "future"} {
		t.Run(name, func(t *testing.T) {
			svc, repo, filter := newAvailabilityProbeFixture()
			probe := &repo.rows[0]
			switch name {
			case "no probes":
				repo.rows = nil
			case "never executed":
				probe.CheckedAt, probe.SampleCount, probe.PassedCount = nil, 0, 0
			case "missing time":
				probe.CheckedAt = nil
			case "inconclusive":
				probe.PassedCount, probe.InconclusiveCount = 0, 1
			case "stale":
				checked := svc.now().Add(-33 * time.Minute)
				probe.CheckedAt = &checked
			case "disabled":
				probe.Enabled = false
			case "manual":
				probe.Type, probe.Enabled = "state_probe", false
			case "runtime off":
				svc.SetRuntimeReader(availabilityProbeRuntime{})
			case "missing runtime":
				svc.SetRuntimeReader(nil)
			case "candy collection off":
				repo.config.Enabled = false
			case "outside window":
				filter.End = *probe.CheckedAt
			case "future":
				checked := svc.now().Add(time.Second)
				probe.CheckedAt = &checked
			}
			snapshot, err := svc.Snapshot(context.Background(), filter, true)
			require.NoError(t, err)
			require.Nil(t, snapshot.Metrics.ProbeAvailability)
			require.Equal(t, "unknown", snapshot.Metrics.AvailabilitySource)
			require.Equal(t, "unknown", snapshot.Health.Overall)
			require.Nil(t, snapshot.Health.Score)
		})
	}
}

func TestChannelMonitorV2ProbeRuntimeModes(t *testing.T) {
	for _, kind := range []string{"connectivity", "quota", "quality", "state_probe", "candy"} {
		for _, mode := range []string{ChannelMonitorModeV1, ChannelMonitorModeV2, ChannelMonitorModeHybrid} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				svc, repo, filter := newAvailabilityProbeFixture()
				repo.rows[0].Type = kind
				svc.SetRuntimeReader(availabilityProbeRuntime{ChannelMonitorRuntime{Enabled: true, Mode: mode}})
				snapshot, err := svc.Snapshot(context.Background(), filter, true)
				require.NoError(t, err)
				allowed := mode == ChannelMonitorModeHybrid || (kind == "connectivity" || kind == "quota") == (mode == ChannelMonitorModeV1)
				require.Equal(t, allowed, snapshot.Metrics.ProbeSampleCount > 0)
			})
		}
	}
}

func TestChannelMonitorV2ProbeFreshnessFollowsSchedule(t *testing.T) {
	checked := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		interval int
		schedule string
		age      time.Duration
		fresh    bool
	}{
		{"short interval grace", 10, "", 3 * time.Minute, true},
		{"missed short interval", 10, "", 3*time.Minute + time.Second, false},
		{"long interval", 1800, "", 31 * time.Minute, true},
		{"missed long interval", 1800, "", 33 * time.Minute, false},
		{"hourly cron", 0, "CRON_TZ=UTC 0 * * * *", 61 * time.Minute, true},
		{"missed cron", 0, "CRON_TZ=UTC 0 * * * *", 63 * time.Minute, false},
		{"timezone cron", 0, "CRON_TZ=Asia/Shanghai 0 21 * * *", 61 * time.Minute, true},
		{"missed timezone cron", 0, "CRON_TZ=Asia/Shanghai 0 21 * * *", 63 * time.Minute, false},
		{"invalid cron fallback", 0, "invalid", 10 * time.Minute, true},
		{"invalid cron expires", 0, "invalid", 16 * time.Minute, false},
		{"unscheduled expires", 0, "", 16 * time.Minute, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			row := ChannelMonitorObservation{CheckedAt: &checked, SampleCount: 1, IntervalSeconds: tt.interval, Schedule: tt.schedule}
			require.Equal(t, tt.fresh, availabilityProbeFresh(row, checked.Add(tt.age)))
		})
	}
}

func TestChannelMonitorV2ProbeBusinessPriorityAndPublicPrivacy(t *testing.T) {
	svc, repo, filter := newAvailabilityProbeFixture()
	repo.rows[0].PassedCount, repo.rows[0].FailedCount = 0, 1
	business := ChannelMonitorV2Metric{HasSamples: true, RequestCount: 100, SuccessRequests: 99, ErrorRequests: 1, ErrorRate: 0.01, TokenCount: 1000, RPM: 2, TPM: 20}
	health := ChannelMonitorV2HealthFor(business)
	repo.snap.Metrics, repo.snap.Health = business, health
	admin, err := svc.Snapshot(context.Background(), filter, true)
	require.NoError(t, err)
	require.Equal(t, "business", admin.Metrics.AvailabilitySource)
	require.Nil(t, admin.Metrics.ProbeAvailability)
	require.Equal(t, business.RequestCount, admin.Metrics.RequestCount)
	require.Equal(t, business.ErrorRate, admin.Metrics.ErrorRate)
	require.Equal(t, business.TokenCount, admin.Metrics.TokenCount)
	require.Equal(t, health, admin.Health)

	filter.RestrictGroups, filter.AllowedGroupIDs = true, []int64{7}
	public, err := svc.Snapshot(context.Background(), filter, false)
	require.NoError(t, err)
	require.True(t, public.Metrics.HasSamples)
	require.Equal(t, "business", public.Metrics.AvailabilitySource)
	require.Zero(t, public.Metrics.ProbeSampleCount)
	require.Zero(t, public.Metrics.ProbeFailedCount)
	require.Zero(t, public.Metrics.RequestCount)
	require.Zero(t, public.Metrics.TokenCount)

	repo.snap.Metrics, repo.snap.Health = ChannelMonitorV2Metric{}, ChannelMonitorV2HealthFor(ChannelMonitorV2Metric{})
	public, err = svc.Snapshot(context.Background(), filter, false)
	require.NoError(t, err)
	require.Equal(t, "unknown", public.Metrics.AvailabilitySource)
	require.Nil(t, public.Metrics.ProbeAvailability)
	require.Equal(t, repo.rows[0].CheckedAt, public.Metrics.ProbeCheckedAt)
	require.False(t, public.Metrics.HasSamples)
	require.Zero(t, public.Metrics.ProbeFailedCount)
	require.Equal(t, "unknown", public.Health.Overall)
}

func TestChannelMonitorV2ProbeOnlyDimensionsAndDeduplication(t *testing.T) {
	svc, repo, filter := newAvailabilityProbeFixture()
	repo.config.Platforms[0].Models = []string{"named"}
	repo.rows = append(repo.rows, repo.rows[0]) // duplicate observation must not inflate any dimension
	models, err := svc.Models(context.Background(), filter, true)
	require.NoError(t, err)
	require.Len(t, models.Items, 1)
	require.Equal(t, ChannelMonitorV2OtherModel, models.Items[0].Model)
	require.EqualValues(t, 1, models.Items[0].Metrics.ProbeSampleCount)
	for _, by := range []ChannelMonitorV2GroupBy{ChannelMonitorV2GroupByPlatform, ChannelMonitorV2GroupByPlatformModel, ChannelMonitorV2GroupByPlatformGroup, ChannelMonitorV2GroupByPlatformGroupModel} {
		t.Run(string(by), func(t *testing.T) {
			matrix, err := svc.Matrix(context.Background(), filter, by, true)
			require.NoError(t, err)
			grouped := by == ChannelMonitorV2GroupByPlatformGroup || by == ChannelMonitorV2GroupByPlatformGroupModel
			if grouped {
				require.Len(t, matrix.Items, 2)
			} else {
				require.Len(t, matrix.Items, 1)
			}
			for _, row := range matrix.Items {
				require.EqualValues(t, 1, row.Metrics.ProbeSampleCount)
				require.Equal(t, "unknown", row.Health.Overall)
				require.Empty(t, row.Buckets)
				if grouped {
					require.NotNil(t, row.GroupID)
					require.Equal(t, repo.rows[0].Scope.GroupNames[*row.GroupID], row.GroupName)
				} else {
					require.Nil(t, row.GroupID)
				}
			}
		})
	}
	filter.RestrictGroups, filter.AllowedGroupIDs = true, []int64{7}
	public, err := svc.Matrix(context.Background(), filter, ChannelMonitorV2GroupByPlatformGroupModel, false)
	require.NoError(t, err)
	require.Len(t, public.Items, 1)
	require.EqualValues(t, 7, *public.Items[0].GroupID)
	require.Nil(t, public.Items[0].Metrics.ProbeAvailability)
	require.Zero(t, public.Items[0].Metrics.ProbeSampleCount)
	require.Zero(t, public.Items[0].Metrics.ProbePassedCount)

	publicModels, err := svc.Models(context.Background(), filter, false)
	require.NoError(t, err)
	require.Len(t, publicModels.Items, 1)
	require.Nil(t, publicModels.Items[0].Metrics.ProbeAvailability)
	require.Zero(t, publicModels.Items[0].Metrics.ProbeSampleCount)
	for i := range repo.rows {
		repo.rows[i].Scope.GroupIDs = nil
	}
	matrix, err := svc.Matrix(context.Background(), ChannelMonitorV2Filter{}, ChannelMonitorV2GroupByPlatformGroup, true)
	require.NoError(t, err)
	require.Empty(t, matrix.Items, "unbound probes must not create fake groups")
}

func TestChannelMonitorV2ProbeAvailabilityFiltering(t *testing.T) {
	for _, name := range []string{"allowed", "denied", "no allowed groups", "configured denied", "selected denied", "platform denied", "platform disabled", "model denied", "other model", "named model"} {
		t.Run(name, func(t *testing.T) {
			svc, repo, filter := newAvailabilityProbeFixture()
			filter.RestrictGroups, filter.AllowedGroupIDs = true, []int64{7}
			repo.config.Platforms[0].Models = []string{"named"}
			want := false
			switch name {
			case "allowed":
				want = true
			case "denied":
				filter.AllowedGroupIDs = []int64{9}
			case "no allowed groups":
				filter.AllowedGroupIDs = nil
			case "configured denied":
				repo.config.GroupIDs = []int64{8}
			case "selected denied":
				filter.GroupIDs = []int64{8}
			case "platform denied":
				filter.Platforms = []string{PlatformAnthropic}
			case "platform disabled":
				repo.config.Platforms[0].Enabled = false
			case "model denied":
				filter.Models = []string{"named"}
			case "other model":
				filter.Models, want = []string{ChannelMonitorV2OtherModel}, true
			case "named model":
				repo.rows[0].Model = "named"
				filter.Models, want = []string{"named"}, true
			}
			snapshot, err := svc.Snapshot(context.Background(), filter, false)
			require.NoError(t, err)
			require.Nil(t, snapshot.Metrics.ProbeAvailability)
			models, err := svc.Models(context.Background(), filter, false)
			require.NoError(t, err)
			require.Equal(t, want, len(models.Items) > 0)
			matrix, err := svc.Matrix(context.Background(), filter, ChannelMonitorV2GroupByPlatformGroupModel, false)
			require.NoError(t, err)
			require.Equal(t, want, len(matrix.Items) > 0)
		})
	}
}

func TestChannelMonitorV2ProbesMergeExistingRows(t *testing.T) {
	svc, repo, filter := newAvailabilityProbeFixture()
	groupID := int64(7)
	repo.matrix.Items = []ChannelMonitorV2MatrixRow{{Platform: PlatformOpenAI, GroupID: &groupID, GroupName: "Visible", Buckets: []ChannelMonitorV2TrendPoint{}}}
	repo.models = []ChannelMonitorV2ModelRow{{Platform: PlatformOpenAI, Model: "probe-only"}}
	models, err := svc.Models(context.Background(), filter, true)
	require.NoError(t, err)
	require.Len(t, models.Items, 1)
	require.Equal(t, "unknown", models.Items[0].Health.Overall)
	matrix, err := svc.Matrix(context.Background(), filter, ChannelMonitorV2GroupByPlatformGroup, true)
	require.NoError(t, err)
	require.Len(t, matrix.Items, 2)
	for _, row := range matrix.Items {
		require.EqualValues(t, 1, row.Metrics.ProbeSampleCount)
	}
}

func TestChannelMonitorV2ProbeLoadFailureIsNotReportedAsHealthy(t *testing.T) {
	svc, repo, filter := newAvailabilityProbeFixture()
	repo.err = errors.New("observation read failed")
	_, err := svc.Snapshot(context.Background(), filter, true)
	require.ErrorIs(t, err, repo.err)
	_, err = svc.Models(context.Background(), filter, true)
	require.ErrorIs(t, err, repo.err)
	_, err = svc.Matrix(context.Background(), filter, ChannelMonitorV2GroupByPlatform, true)
	require.ErrorIs(t, err, repo.err)
}

func TestChannelMonitorV2ExpiredProbeClearsPreviousHealth(t *testing.T) {
	metric := ChannelMonitorV2Metric{}
	health := ChannelMonitorV2Health{}
	(channelMonitorProbeAvailability{samples: 1, passed: 1}).apply(&metric, &health, DefaultChannelMonitorV2HealthThresholds())
	require.Equal(t, "unknown", health.Overall)
	(channelMonitorProbeAvailability{}).apply(&metric, &health, DefaultChannelMonitorV2HealthThresholds())
	require.Equal(t, "unknown", health.Overall)
	require.Nil(t, health.Score)
	require.Nil(t, metric.ProbeAvailability)
	require.Equal(t, "unknown", metric.AvailabilitySource)
}

func TestChannelMonitorV2ObservationMetadataPreservesUnifiedRequestSource(t *testing.T) {
	for _, source := range []string{"business", "probe", "mixed"} {
		metric := ChannelMonitorV2Metric{HasSamples: true, RequestCount: 10, AvailabilitySource: source}
		health := ChannelMonitorV2HealthFor(metric)
		before := health
		(channelMonitorProbeAvailability{samples: 99, failed: 99}).apply(&metric, &health, DefaultChannelMonitorV2HealthThresholds())
		require.Equal(t, source, metric.AvailabilitySource)
		require.EqualValues(t, 10, metric.RequestCount)
		require.Equal(t, before, health)
		require.Nil(t, metric.ProbeAvailability)
	}
	svc, repo, filter := newAvailabilityProbeFixture()
	filter.Source = "business"
	snapshot, err := svc.Snapshot(context.Background(), filter, true)
	require.NoError(t, err)
	require.Zero(t, snapshot.Metrics.ProbeSampleCount)
	require.NotEmpty(t, repo.rows)
}

func TestChannelMonitorV2RedactionKeepsRealProbeSamplesButNoSourceVolumes(t *testing.T) {
	metric := ChannelMonitorV2Metric{HasSamples: true, RequestCount: 12, BusinessRequestCount: 2, ProbeRequestCount: 10, UsageIncompleteRequestCount: 3, CostIncompleteRequestCount: 4, AvailabilitySource: "mixed", ErrorRate: 0.25}
	redactChannelMonitorV2Metric(&metric, true)
	require.True(t, metric.HasSamples)
	require.Equal(t, "mixed", metric.AvailabilitySource)
	require.Equal(t, 0.25, metric.ErrorRate)
	require.Zero(t, metric.BusinessRequestCount)
	require.Zero(t, metric.ProbeRequestCount)
	require.Zero(t, metric.UsageIncompleteRequestCount)
	require.Zero(t, metric.CostIncompleteRequestCount)
}

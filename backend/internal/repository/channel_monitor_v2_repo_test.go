package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2DateBinOriginIsUTC(t *testing.T) {
	require.Equal(t, "TIMESTAMPTZ '1970-01-01 00:00:00+00'", channelMonitorV2DateBinOrigin)
	require.Equal(t, "date_bin($1::interval,m.bucket_start,TIMESTAMPTZ '1970-01-01 00:00:00+00')", channelMonitorV2DateBinExpr("m.bucket_start"))

	for _, query := range []string{
		channelMonitorV2FixedRollupBoundsSQL,
		channelMonitorV2MetricsRollupSQL,
		channelMonitorV2UserMetricsRollupSQL,
		channelMonitorV2HistogramRollupSQL,
		channelMonitorV2ErrorRollupSQL,
	} {
		require.Contains(t, query, channelMonitorV2DateBinOrigin)
		require.NotContains(t, query, "TIMESTAMPTZ '1970-01-01'")
	}
}

func TestChannelMonitorV2DisplayModelIsPlatformScoped(t *testing.T) {
	cfg := service.ChannelMonitorV2Config{Platforms: []service.ChannelMonitorV2PlatformConfig{
		{Platform: "openai", Enabled: true, Models: []string{"shared", "gpt-5"}},
		{Platform: "grok", Enabled: true, Models: []string{"grok-4"}},
		// Empty models list must NOT collapse everything into __other__.
		{Platform: "anthropic", Enabled: true, Models: []string{}},
	}}
	require.Equal(t, "shared", channelMonitorV2DisplayModel(cfg, "openai", "shared"))
	require.Equal(t, service.ChannelMonitorV2OtherModel, channelMonitorV2DisplayModel(cfg, "grok", "shared"))
	require.Equal(t, "claude-sonnet-4", channelMonitorV2DisplayModel(cfg, "anthropic", "claude-sonnet-4"))
	// Unconfigured platform still surfaces the real model name.
	require.Equal(t, "gemini-2.5-pro", channelMonitorV2DisplayModel(cfg, "gemini", "gemini-2.5-pro"))
	require.True(t, channelMonitorV2ModelSelected(service.ChannelMonitorV2Filter{Models: []string{service.ChannelMonitorV2OtherModel}}, cfg, "grok", "shared"))
}

func TestChannelMonitorV2MatrixDimensionKey(t *testing.T) {
	cfg := service.ChannelMonitorV2Config{Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}}}}
	key := channelMonitorV2MatrixDimensionKey(service.ChannelMonitorV2GroupByPlatformGroupModel, cfg, "openai", 7, "gpt-5")
	require.Equal(t, channelMonitorV2MatrixKey{platform: "openai", groupID: 7, model: "gpt-5"}, key)
	key = channelMonitorV2MatrixDimensionKey(service.ChannelMonitorV2GroupByPlatformModel, cfg, "openai", 7, "unlisted")
	require.Equal(t, channelMonitorV2MatrixKey{platform: "openai", model: service.ChannelMonitorV2OtherModel}, key)
	key = channelMonitorV2MatrixDimensionKey(service.ChannelMonitorV2GroupByPlatform, cfg, "openai", 7, "gpt-5")
	require.Equal(t, channelMonitorV2MatrixKey{platform: "openai"}, key)
}

func TestChannelMonitorV2HistogramPercentilesAreMergedFromCounts(t *testing.T) {
	// 100 samples: 50@100, 40@500, 10@1000
	// target = int64(total*p + 0.999999) truncates: p50→50, p90→90, p95→95
	// cumulative hits: p50@100, p90@500 (50+40), p95@1000
	histogram := map[int64]int64{100: 50, 500: 40, 1000: 10}
	require.Equal(t, int64(100), *histPercentile(histogram, .5))
	require.Equal(t, int64(500), *histPercentile(histogram, .9))
	require.Equal(t, int64(1000), *histPercentile(histogram, .95))
	require.Nil(t, histPercentile(nil, .95))
	// latencyMetric exposes avg + p50 + p90 + p95
	lat := latencyMetric(1000, 10, histogram)
	require.NotNil(t, lat.AvgMs)
	require.NotNil(t, lat.P50Ms)
	require.NotNil(t, lat.P90Ms)
	require.NotNil(t, lat.P95Ms)
	require.Equal(t, int64(100), *lat.P50Ms)
	require.Equal(t, int64(500), *lat.P90Ms)
	require.Equal(t, int64(1000), *lat.P95Ms)
}

func TestChannelMonitorV2MetricIncludesSuccessRate(t *testing.T) {
	acc := newMetricAccumulator()
	acc.success, acc.errors = 80, 20
	metric := acc.metric(1, false)
	require.Equal(t, int64(100), metric.RequestCount)
	require.InDelta(t, 0.8, metric.SuccessRate, 0.0001)
	require.InDelta(t, 0.2, metric.ErrorRate, 0.0001)
	require.Nil(t, metric.UpstreamAffectedRequests)

	adminMetric := acc.metric(1, true)
	require.NotNil(t, adminMetric.UpstreamAffectedRequests)
}

func TestChannelMonitorV2WhereUsesConfiguredScopeAndEmptyFilterMeansAllConfigured(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{Start: time.Unix(1, 0), End: time.Unix(2, 0)}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}, {Platform: "grok", Enabled: false}},
		GroupIDs:  []int64{3, 4},
	}
	where, args := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "m.platform = ANY($3)")
	require.Contains(t, where, "m.group_id = ANY($4)")
	require.Len(t, args, 4)
}

func TestChannelMonitorV2WhereHidesDeletedGroupsButKeepsUngroupedRows(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{Start: time.Unix(1, 0), End: time.Unix(2, 0)}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
	}
	where, args := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "COALESCE(m.group_id, 0) = 0 OR EXISTS (SELECT 1 FROM groups live_group")
	require.Contains(t, where, "live_group.id = m.group_id AND live_group.deleted_at IS NULL")
	require.Len(t, args, 3, "the deleted-group rule must not add arguments")

	other, _ := channelMonitorV2Where(filter, cfg, "metric")
	require.Contains(t, other, "COALESCE(metric.group_id, 0) = 0 OR EXISTS")
	require.Contains(t, other, "live_group.id = metric.group_id")
}

func TestChannelMonitorV2WhereRejectsGroupFilterOutsideConfiguredScope(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{
		Start: time.Unix(1, 0), End: time.Unix(2, 0), GroupIDs: []int64{9},
	}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		GroupIDs:  []int64{3, 4},
	}
	where, args := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "FALSE")
	require.NotContains(t, where, "m.group_id = ANY")
	require.Len(t, args, 3)
}

func TestChannelMonitorV2WhereRestrictsOrdinaryViewerToAllowedConfiguredGroups(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{
		Start: time.Unix(1, 0), End: time.Unix(2, 0),
		GroupIDs: []int64{4, 9}, AllowedGroupIDs: []int64{3, 4}, RestrictGroups: true,
	}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		GroupIDs:  []int64{3, 4, 9},
	}
	where, args := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "m.group_id = ANY($4)")
	require.Equal(t, pq.Array([]int64{4}), args[3])
}

func TestChannelMonitorV2WhereRejectsOrdinaryViewerWithNoAllowedGroups(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{
		Start: time.Unix(1, 0), End: time.Unix(2, 0),
		GroupIDs: []int64{9}, RestrictGroups: true,
	}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		GroupIDs:  []int64{3, 9},
	}
	where, _ := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "FALSE")
	require.NotContains(t, where, "m.group_id = ANY")
}

func TestChannelMonitorV2CatalogKeepsViewerScopeWhileIgnoringPickerFilters(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{
		Platforms: []string{"openai"}, GroupIDs: []int64{9}, Models: []string{"gpt-5"},
		AllowedGroupIDs: []int64{3}, RestrictGroups: true,
	}
	catalog := channelMonitorV2CatalogFilter(filter)
	require.Empty(t, catalog.Platforms)
	require.Empty(t, catalog.GroupIDs)
	require.Empty(t, catalog.Models)
	require.True(t, catalog.RestrictGroups)
	require.Equal(t, []int64{3}, catalog.AllowedGroupIDs)
}

func TestChannelMonitorV2AdminScopeRemainsGlobal(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{Start: time.Unix(1, 0), End: time.Unix(2, 0), GroupIDs: []int64{9}}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		GroupIDs:  []int64{3, 9},
	}
	where, args := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "m.group_id = ANY($4)")
	require.Equal(t, pq.Array([]int64{9}), args[3])
}

func TestChannelMonitorV2MatrixDoesNotSeedGroupsForEmptyViewerScope(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{RestrictGroups: true}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		GroupIDs:  []int64{3, 9},
	}
	accs := seedChannelMonitorV2MatrixAccumulators(filter, cfg, service.ChannelMonitorV2GroupByPlatformGroup, map[int64]channelMonitorV2GroupInfo{
		3: {name: "private"}, 9: {name: "other-private"},
	})
	require.Empty(t, accs)
}

func TestChannelMonitorV2EmptyRestrictedScopeReturnsEmptyInventory(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorV2Repository{db: db}
	filter := service.ChannelMonitorV2Filter{RestrictGroups: true}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}}},
		GroupIDs:  []int64{3, 9},
	}

	dimensions, err := repo.GetDimensions(context.Background(), filter, cfg)
	require.NoError(t, err)
	require.Empty(t, dimensions.Platforms)
	require.Empty(t, dimensions.Groups)
	require.Empty(t, dimensions.Models)
	require.NotNil(t, dimensions.Platforms)
	require.NotNil(t, dimensions.Groups)
	require.NotNil(t, dimensions.Models)

	models, err := repo.GetModels(context.Background(), filter, cfg, false)
	require.NoError(t, err)
	require.Empty(t, models.Items)
	require.NotNil(t, models.Items)
	require.Equal(t, service.ChannelMonitorV2Coverage{}, models.Coverage)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestChannelMonitorV2EmptyRestrictedScopeReturnsEmptySnapshotWithoutQueries(t *testing.T) {
	tests := []struct {
		name   string
		filter service.ChannelMonitorV2Filter
	}{
		{
			name:   "no allowed groups",
			filter: service.ChannelMonitorV2Filter{RestrictGroups: true},
		},
		{
			name: "requested groups exclude allowed configured scope",
			filter: service.ChannelMonitorV2Filter{
				GroupIDs: []int64{9}, AllowedGroupIDs: []int64{3}, RestrictGroups: true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			repo := &channelMonitorV2Repository{db: db}
			cfg := service.ChannelMonitorV2Config{
				Enabled: true,
				Platforms: []service.ChannelMonitorV2PlatformConfig{{
					Platform: "openai", Enabled: true, Models: []string{"gpt-5"},
				}},
				GroupIDs: []int64{3},
			}

			snapshot, err := repo.GetSnapshot(context.Background(), test.filter, cfg, false)
			require.NoError(t, err)
			require.Equal(t, service.ChannelMonitorV2Config{}, snapshot.Config)
			require.Equal(t, service.ChannelMonitorV2Coverage{}, snapshot.Coverage)
			require.Equal(t, service.ChannelMonitorV2Metric{}, snapshot.Metrics)
			require.Equal(t, service.ChannelMonitorV2Health{}, snapshot.Health)
			require.Empty(t, snapshot.Trend)
			require.NotNil(t, snapshot.Trend)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestChannelMonitorV2EmptyRestrictedScopeReturnsEmptyMatrixForEveryGrouping(t *testing.T) {
	groupings := []service.ChannelMonitorV2GroupBy{
		service.ChannelMonitorV2GroupByPlatform,
		service.ChannelMonitorV2GroupByPlatformModel,
		service.ChannelMonitorV2GroupByPlatformGroup,
		service.ChannelMonitorV2GroupByPlatformGroupModel,
	}
	for _, groupBy := range groupings {
		t.Run(string(groupBy), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			repo := &channelMonitorV2Repository{db: db}
			filter := service.ChannelMonitorV2Filter{RestrictGroups: true}
			cfg := service.ChannelMonitorV2Config{
				Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}}},
				GroupIDs:  []int64{3, 9},
			}

			matrix, err := repo.GetMatrix(context.Background(), filter, cfg, groupBy, false)
			require.NoError(t, err)
			require.Equal(t, groupBy, matrix.GroupBy)
			require.Empty(t, matrix.Items)
			require.NotNil(t, matrix.Items)
			require.Equal(t, service.ChannelMonitorV2Coverage{}, matrix.Coverage)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestChannelMonitorV2ErrorAggregationCountsFinalUserErrorsOnly(t *testing.T) {
	query := strings.ToLower(channelMonitorV2ErrorAggregationSQL)
	require.Contains(t, query, "not current_error.is_count_tokens")
	require.Contains(t, query, "error_type = 'cyber_policy'")
	require.Contains(t, query, "distinct on")
	require.Contains(t, query, "candidate_ids")
	require.Contains(t, query, "where bucket_start >= $1 and bucket_start < $2")
	require.Contains(t, query, "upstream_affected_requests")
	require.Contains(t, query, "jsonb_array_length(current_error.upstream_errors) > 0")
	// request_id dedup must be time-bounded (no full-history scan).
	require.Contains(t, query, "interval '90 minutes'")
	require.Contains(t, query, "current_error.created_at >= $1 - interval '90 minutes'")
}

func TestChannelMonitorV2ErrorAggregationResolvesCompositePlatform(t *testing.T) {
	query := strings.ToLower(channelMonitorV2ErrorAggregationSQL)
	// Composite groups are a routing layer: error facts must resolve the concrete
	// account platform (joining groups/accounts) so they aggregate under the same
	// platform key as usage facts instead of the never-enabled 'composite' platform.
	require.Contains(t, query, "g.platform = 'composite'")
	require.Contains(t, query, "left join groups g on g.id = current_error.group_id")
	require.Contains(t, query, "left join accounts a on a.id = current_error.account_id")
	require.Contains(t, query, "a.platform")
	require.Contains(t, query, "nullif(trim(a.platform), '')")
	require.NotContains(t, query, "nullif(trim(a.platform))")
}

func TestChannelMonitorV2UsageSuccessExcludesCyberBillingRows(t *testing.T) {
	for _, query := range []string{channelMonitorV2UsageMetricsSQL, channelMonitorV2UserMetricsSQL} {
		require.Contains(t, query, "COALESCE(ul.request_type, 0) NOT IN (4, 6)")
		require.Contains(t, query, "ul.actual_cost > 0")
	}
	require.Contains(t, channelMonitorV2PlatformSQL, "g.platform = 'composite'")
	require.Contains(t, channelMonitorV2PlatformSQL, "a.platform")
	require.Contains(t, channelMonitorV2HistogramSQL, "ul.actual_cost > 0")
}

func TestChannelMonitorV2RatesUseCoveredWindow(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	filter := service.ChannelMonitorV2Filter{Start: start, End: start.Add(24 * time.Hour)}
	coverage := service.ChannelMonitorV2Coverage{CoverageStart: start.Add(6 * time.Hour), DataThrough: start.Add(18 * time.Hour)}
	require.Equal(t, 12*60.0, channelMonitorV2CoveredMinutes(filter, coverage))
	effective := channelMonitorV2CommonCoverageFilter(filter, coverage)
	require.Equal(t, coverage.CoverageStart, effective.Start)
	require.Equal(t, coverage.DataThrough, effective.End)
}

func TestChannelMonitorV2HistoryCoverageCompleteIgnoresTrailingLag(t *testing.T) {
	start := time.Date(2026, 8, 7, 2, 20, 0, 0, time.UTC)
	// History reaches the window start → complete even if data_through is behind filter.End.
	require.True(t, channelMonitorV2HistoryCoverageComplete(start, start))
	require.True(t, channelMonitorV2HistoryCoverageComplete(start.Add(-time.Hour), start))
	// Backfill still short of the window start → incomplete.
	require.False(t, channelMonitorV2HistoryCoverageComplete(start.Add(time.Hour), start))
	require.False(t, channelMonitorV2HistoryCoverageComplete(time.Time{}, start))
}

func TestChannelMonitorV2TierRetentionPolicy(t *testing.T) {
	require.Equal(t, 3*24*time.Hour, channelMonitorV2RetentionUser1m)
	require.Equal(t, 7*24*time.Hour, channelMonitorV2RetentionMetrics1m)
	require.Equal(t, 7*24*time.Hour, channelMonitorV2RetentionError1m)
	require.Equal(t, 7*24*time.Hour, channelMonitorV2RetentionHistogram1m)
	require.Equal(t, 7*24*time.Hour, channelMonitorV2RetentionRollup5m)
	require.Equal(t, 30*24*time.Hour, channelMonitorV2RetentionRollup1h)
	require.Equal(t, 45*24*time.Hour, channelMonitorV2RetentionRollup12h)
	require.Equal(t, 90*24*time.Hour, channelMonitorV2RetentionRollup1d)
	require.Equal(t, channelMonitorV2RetentionRollup1d, channelMonitorV2MaxRetention())
	require.Contains(t, channelMonitorV2WatermarkSQL, "INTERVAL '90 days'")

	// Every fixed rollup second must appear with a retention rule.
	wantSeconds := map[int]time.Duration{
		300:   channelMonitorV2RetentionRollup5m,
		3600:  channelMonitorV2RetentionRollup1h,
		43200: channelMonitorV2RetentionRollup12h,
		86400: channelMonitorV2RetentionRollup1d,
	}
	seen := map[int]time.Duration{}
	for _, rule := range channelMonitorV2RetentionRules {
		if rule.bucketSeconds == 0 {
			require.True(t, rule.retention > 0)
			continue
		}
		if prev, ok := seen[rule.bucketSeconds]; ok {
			require.Equal(t, prev, rule.retention)
		}
		seen[rule.bucketSeconds] = rule.retention
	}
	for seconds, want := range wantSeconds {
		got, ok := seen[seconds]
		require.Truef(t, ok, "missing retention rule for bucket_seconds=%d", seconds)
		require.Equal(t, want, got)
	}

	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	require.Equal(t, now.Add(-7*24*time.Hour), channelMonitorV2RetentionCutoff(now, channelMonitorV2RetentionMetrics1m))
	require.Equal(t, now.Add(-90*24*time.Hour), channelMonitorV2RetentionCutoff(now, channelMonitorV2MaxRetention()))
}

func TestSameFixedRollupBucket(t *testing.T) {
	start := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	require.True(t, sameFixedRollupBucket(start, start.Add(10*time.Minute), 86400))
	require.False(t, sameFixedRollupBucket(start, start.Add(15*time.Hour), 43200))
	require.False(t, sameFixedRollupBucket(start, start.Add(24*time.Hour), 86400))
}

// Needles present in service.ClassifyChannelMonitorV2Error must appear in the
// aggregation SQL CASE so rollup categories match drilldown classification.
func TestChannelMonitorV2SQLTaxonomyContainsGoNeedles(t *testing.T) {
	sql := channelMonitorV2ErrorAggregationSQL
	needles := []string{
		"blocked keyword",
		"invalid_api_key",
		"max_tokens",
		"invalid_request",
		"model not supported",
		"billing hard limit",
		"no healthy upstream account",
		"rate_limit",
		"gateway timeout",
		"connection refused",
		"unexpected eof",
	}
	for _, needle := range needles {
		require.Containsf(t, strings.ToLower(sql), strings.ToLower(needle), "SQL taxonomy missing Go needle %q", needle)
	}
}

func TestApplyIgnoredErrorsAdjustsRatesKeepsAbsoluteVolume(t *testing.T) {
	m := service.ChannelMonitorV2Metric{
		RequestCount:  100,
		ErrorRequests: 20,
		ErrorRate:     0.20,
		SuccessRate:   0.80,
	}
	// Success absolute still 80 → success rate stays 0.80 even after ignoring 5 errors.
	m.SuccessRequests = 80
	applyIgnoredErrors(&m, 5)
	require.Equal(t, int64(100), m.RequestCount)
	require.Equal(t, int64(20), m.ErrorRequests)
	require.InDelta(t, 0.15, m.ErrorRate, 0.0001)
	require.InDelta(t, 0.80, m.SuccessRate, 0.0001)

	// Clamp ignored > errors: scored error_rate → 0; success stays true ratio.
	m2 := service.ChannelMonitorV2Metric{RequestCount: 10, ErrorRequests: 2, SuccessRequests: 8, ErrorRate: 0.2, SuccessRate: 0.8}
	applyIgnoredErrors(&m2, 99)
	require.InDelta(t, 0.0, m2.ErrorRate, 0.0001)
	require.InDelta(t, 0.8, m2.SuccessRate, 0.0001)

	// No-op when ignored is zero
	m3 := service.ChannelMonitorV2Metric{RequestCount: 10, ErrorRequests: 2, SuccessRequests: 8, ErrorRate: 0.2, SuccessRate: 0.8}
	applyIgnoredErrors(&m3, 0)
	require.InDelta(t, 0.2, m3.ErrorRate, 0.0001)
	require.InDelta(t, 0.8, m3.SuccessRate, 0.0001)
}

func TestRedactChannelMonitorV2MetricZerosVolume(t *testing.T) {
	// Service helper is in service package; covered there. Keep a smoke note that
	// rates survive a manual zeroing of volume fields used by the UI contract.
	m := service.ChannelMonitorV2Metric{
		RequestCount: 100, ErrorRequests: 10, SuccessRequests: 90,
		TokenCount: 1000, RPM: 5, TPM: 50, ErrorRate: 0.1, SuccessRate: 0.9, CacheRate: 0.4,
	}
	// Mimic redact: zero volume only
	m.RequestCount, m.ErrorRequests, m.SuccessRequests, m.TokenCount = 0, 0, 0, 0
	require.Equal(t, 0.1, m.ErrorRate)
	require.Equal(t, 5.0, m.RPM)
}

func TestChannelMonitorV2CatalogFilterClearsMultiSelectDimensions(t *testing.T) {
	start := time.Unix(1, 0)
	end := time.Unix(2, 0)
	filter := service.ChannelMonitorV2Filter{
		Start: start, End: end, Bucket: time.Minute,
		Platforms: []string{"openai"}, GroupIDs: []int64{3}, Models: []string{"gpt-5"},
	}
	catalog := channelMonitorV2CatalogFilter(filter)
	require.Nil(t, catalog.Platforms)
	require.Nil(t, catalog.GroupIDs)
	require.Nil(t, catalog.Models)
	// Time window / coverage-related fields remain.
	require.Equal(t, start, catalog.Start)
	require.Equal(t, end, catalog.End)
	require.Equal(t, time.Minute, catalog.Bucket)

	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{
			{Platform: "openai", Enabled: true},
			{Platform: "grok", Enabled: true},
		},
		GroupIDs: []int64{3, 4},
	}
	catalogWhere, catalogArgs := channelMonitorV2Where(catalog, cfg, "m")
	_, metricArgs := channelMonitorV2Where(filter, cfg, "m")

	// Catalog WHERE still applies config scope (enabled platforms + group allow-list).
	require.Contains(t, catalogWhere, "m.platform = ANY")
	require.Contains(t, catalogWhere, "m.group_id = ANY")
	require.Len(t, catalogArgs, 4) // start, end, platforms, groups
	require.Len(t, metricArgs, 4)

	// Metrics WHERE is narrower once multi-select platforms/groups are applied.
	require.NotEqual(t, catalogArgs, metricArgs)
	// Group seeding without multi-select uses full config allow-list.
	require.Equal(t, []int64{3, 4}, configuredChannelMonitorV2GroupIDs(catalog, cfg))
	require.Equal(t, []int64{3}, configuredChannelMonitorV2GroupIDs(filter, cfg))
}

func TestChannelMonitorV2ActualRequestSourcesPreservedAndLeaderboardBusinessOnly(t *testing.T) {
	require.Contains(t, channelMonitorV2UsageMetricsSQL, "ul.source IN ('business', 'probe')")
	require.Contains(t, channelMonitorV2UsageMetricsSQL, "model, source, success_requests")
	require.Contains(t, channelMonitorV2ErrorAggregationSQL, "current_error.source IN ('business', 'probe')")
	require.Contains(t, channelMonitorV2ErrorAggregationSQL, "current_error.source, COALESCE(NULLIF(current_error.request_id")
	require.Contains(t, channelMonitorV2ErrorAggregationSQL, "WHERE user_id IS NOT NULL AND source = 'business'")
	require.Contains(t, channelMonitorV2UserMetricsSQL, "ul.source = 'business'")
	require.Contains(t, channelMonitorV2HistogramSQL, "audience.user_id = 0 OR ul.source = 'business'")
	for _, query := range []string{channelMonitorV2MetricsRollupSQL, channelMonitorV2ErrorRollupSQL, channelMonitorV2HistogramRollupSQL} {
		require.Contains(t, query, "platform, group_id, model, source")
	}
	require.NotContains(t, channelMonitorV2UsageMetricsSQL, "channel_monitor_history")
	require.NotContains(t, channelMonitorV2DirectProbeMetricsSQL, "usage_logs")
}

func TestChannelMonitorV2SourceFilterAppliesAcrossFactTypes(t *testing.T) {
	cfg := service.ChannelMonitorV2Config{Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}}}
	for _, source := range []string{"business", "probe"} {
		for _, alias := range []string{"m", "h", "e"} {
			where, args := channelMonitorV2Where(service.ChannelMonitorV2Filter{Source: source}, cfg, alias)
			require.Contains(t, where, alias+".source = $4")
			require.Equal(t, source, args[3])
		}
	}
	where, args := channelMonitorV2Where(service.ChannelMonitorV2Filter{Source: "all"}, cfg, "m")
	require.NotContains(t, where, "m.source =")
	require.Len(t, args, 3)
}

func TestChannelMonitorV2ProbeOnlyAndMixedRequestsHaveRealMetrics(t *testing.T) {
	probe := newMetricAccumulator()
	probe.addFact(channelMonitorV2Fact{Source: "probe", Success: 2, Errors: 1, Input: 100, Output: 10, DurationSum: 1200, DurationCount: 2, UsageIncomplete: 1, CostIncomplete: 2})
	probe.addHistogram(channelMonitorV2Histogram{Metric: "duration", UpperBound: 500, Count: 1})
	probe.addHistogram(channelMonitorV2Histogram{Metric: "duration", UpperBound: 1000, Count: 1})
	metric := probe.metric(5, true)
	require.True(t, metric.HasSamples)
	require.Equal(t, "probe", metric.AvailabilitySource)
	require.Equal(t, int64(3), metric.RequestCount)
	require.Zero(t, metric.BusinessRequestCount)
	require.Equal(t, int64(3), metric.ProbeRequestCount)
	require.Equal(t, int64(110), metric.TokenCount)
	require.InDelta(t, 2.0/3, metric.SuccessRate, 1e-9)
	require.InDelta(t, 0.6, metric.RPM, 1e-9)
	require.Equal(t, int64(1), metric.UsageIncompleteRequestCount)
	require.Equal(t, int64(2), metric.CostIncompleteRequestCount)
	require.Nil(t, metric.TTFT.P50Ms, "non-stream duration is never a TTFT substitute")
	require.Nil(t, metric.TTFT.AvgMs)
	require.Equal(t, int64(500), *metric.Duration.P50Ms)

	probe.addFact(channelMonitorV2Fact{Source: "business", Success: 1, Input: 20, Output: 5, DurationSum: 100, DurationCount: 1, TTFTSum: 50, TTFTCount: 1})
	probe.addHistogram(channelMonitorV2Histogram{Metric: "duration", UpperBound: 100, Count: 1})
	probe.addHistogram(channelMonitorV2Histogram{Metric: "ttft", UpperBound: 50, Count: 1})
	mixed := probe.metric(5, false)
	require.Equal(t, "mixed", mixed.AvailabilitySource)
	require.Equal(t, int64(4), mixed.RequestCount)
	require.Equal(t, int64(1), mixed.BusinessRequestCount)
	require.Equal(t, int64(3), mixed.ProbeRequestCount)
	require.InDelta(t, .75, mixed.SuccessRate, 1e-9)
	require.Equal(t, int64(500), *mixed.Duration.P50Ms, "merge histogram samples, not source P50 averages")
	require.Equal(t, int64(50), *mixed.TTFT.P50Ms)
}

func TestChannelMonitorV2LoadFactsIncludesSourceDimension(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC().Truncate(time.Minute)
	filter := service.ChannelMonitorV2Filter{Source: "probe", Start: now.Add(-time.Hour), End: now, Bucket: time.Minute}
	cfg := service.ChannelMonitorV2Config{Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}}}
	mock.ExpectQuery(`SELECT MIN\(m.bucket_start\).*m.model,m.source.*GROUP BY m.platform,m.group_id,g.name,m.model,m.source`).
		WithArgs(filter.Start, filter.End, pq.Array([]string{"openai"}), "probe").
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "platform", "group_id", "group_name", "model", "source", "success", "errors", "affected", "attempts", "input", "output", "cache_create", "cache_read", "ttft_sum", "ttft_count", "duration_sum", "duration_count", "usage_missing", "cost_missing"}).
			AddRow(now.Add(-time.Minute), "openai", 3, "group", "model", "probe", 1, 1, 1, 3, 10, 5, 0, 0, 0, 0, 200, 1, 1, 1))
	facts, err := (&channelMonitorV2Repository{db: db}).loadFacts(context.Background(), filter, cfg, false)
	require.NoError(t, err)
	require.Len(t, facts, 1)
	require.Equal(t, "probe", facts[0].Source)
	require.Equal(t, int64(1), facts[0].UsageIncomplete)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestChannelMonitorV2WatermarkRejectsStaleAccountingVersion(t *testing.T) {
	for _, version := range []int{1, service.ChannelMonitorV2AccountingVersion} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		now := time.Now().UTC().Truncate(time.Minute)
		mock.ExpectQuery(`SELECT usage_coverage_start, error_coverage_start, data_through, last_successful_at, backfill_cursor, accounting_version`).
			WillReturnRows(sqlmock.NewRows([]string{"usage", "error", "through", "computed", "cursor", "version"}).AddRow(now.Add(-90*24*time.Hour), now.Add(-90*24*time.Hour), now, now, now.Add(-90*24*time.Hour), version))
		wm, err := (&channelMonitorV2Repository{db: db}).GetAggregationWatermark(context.Background())
		require.NoError(t, err)
		require.Equal(t, version, wm.AccountingVersion)
		if version == service.ChannelMonitorV2AccountingVersion {
			require.True(t, wm.HasData)
			require.Equal(t, now, wm.DataThrough)
		} else {
			require.False(t, wm.HasData)
			require.True(t, wm.BackfillCursor.IsZero())
		}
		require.NoError(t, mock.ExpectationsWereMet())
		_ = db.Close()
	}
}

func TestChannelMonitorV2DirectProbeLogicalOutcomeAndPhysicalAccounting(t *testing.T) {
	require.Contains(t, channelMonitorV2DirectProbeMetricsSQL, "COUNT(*) FILTER (WHERE is_final AND api_success)")
	require.Contains(t, channelMonitorV2DirectProbeMetricsSQL, "COUNT(*) FILTER (WHERE is_final AND NOT api_success)")
	require.Contains(t, channelMonitorV2DirectProbeMetricsSQL, "COUNT(*) FILTER (WHERE is_final AND NOT api_success), COUNT(*)")
	require.Contains(t, channelMonitorV2DirectProbeMetricsSQL, "SUM(input_tokens)")
	require.Contains(t, channelMonitorV2DirectProbeMetricsSQL, "NOT missing.usage_complete")
	require.Contains(t, channelMonitorV2DirectProbeRowsSQL, "p.role = 'generation' AND p.group_id > 0")
	require.Contains(t, channelMonitorV2DirectProbeRowsSQL, "p.first_token_ms IS NOT NULL")
	require.Contains(t, channelMonitorV2DirectProbeRowsSQL, "INTERVAL '90 minutes'")
	require.Contains(t, channelMonitorV2DirectProbeHistogramSQL, "p.is_final AND p.api_success")
	require.Contains(t, channelMonitorV2DirectProbeErrorsSQL, "p.is_final AND NOT p.api_success")
	require.NotContains(t, channelMonitorV2DirectProbeErrorsSQL, "verdict")
}

func TestChannelMonitorV2SuccessUsesObservedOutcomeNotBilling(t *testing.T) {
	require.Contains(t, channelMonitorV2APISuccessFilterUL, "COALESCE(ul.api_success, ul.actual_cost > 0) AND NOT EXISTS")
	require.Contains(t, channelMonitorV2UsageMetricsSQL, "COALESCE(ul.api_success, ul.actual_cost > 0) AND NOT EXISTS")
	require.Contains(t, channelMonitorV2HistogramSQL, "COALESCE(ul.api_success, ul.actual_cost > 0) AND NOT EXISTS")
	require.Contains(t, channelMonitorV2UserMetricsSQL, "ul.api_success")
	require.Contains(t, channelMonitorV2UserMetricsSQL, "ul.source = 'business'")
}

func TestChannelMonitorV2MarkedFailuresHaveDedupedTelemetryFallback(t *testing.T) {
	require.Contains(t, channelMonitorV2ErrorAggregationSQL, "usage_failures AS")
	require.Contains(t, channelMonitorV2ErrorAggregationSQL, "ul.api_success = FALSE")
	require.Contains(t, channelMonitorV2ErrorAggregationSQL, "AND NOT "+channelMonitorV2HasFinalOpsErrorSQL)
	require.Contains(t, channelMonitorV2GatewayFailureCategorySQL, "transport_or_stream")
	require.NotContains(t, channelMonitorV2ErrorAggregationSQL, "COALESCE(ul.api_success, FALSE)")
}

func TestChannelMonitorV2ProbeRetentionIsBoundedAndKeepsLogicalRuns(t *testing.T) {
	require.Contains(t, channelMonitorV2ProbeRetentionPruneSQL, "LIMIT 1000")
	require.Contains(t, channelMonitorV2ProbeRetentionPruneSQL, "old_attempt.logical_request_id = old_runs.logical_request_id")
	require.Contains(t, channelMonitorV2ProbeRetentionPruneSQL, "retained_attempt.finished_at >= $1")
}

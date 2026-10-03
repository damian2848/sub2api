package service

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func cacheSensitiveHealthThresholds() ChannelMonitorV2HealthThresholds {
	thresholds := DefaultChannelMonitorV2HealthThresholds()
	thresholds.WarningCacheRate = 0.85
	thresholds.CriticalCacheRate = 0.60
	return thresholds
}

func TestChannelMonitorV2HealthRequiresRequestSamplesForEverySignal(t *testing.T) {
	thresholds := cacheSensitiveHealthThresholds()
	p50 := int64(30000)
	for _, requests := range []int64{0, 5, 49} {
		t.Run(strconv.FormatInt(requests, 10), func(t *testing.T) {
			health := ChannelMonitorV2HealthForWithThresholds(ChannelMonitorV2Metric{
				RequestCount: requests, ErrorRate: 0, CacheRate: 0,
				CacheRateDenominator: 100000,
				TTFT:                 ChannelMonitorV2Latency{SampleCount: 100, P50Ms: &p50},
			}, thresholds)
			require.Equal(t, "unknown", health.Overall)
			require.Equal(t, "unknown", health.ErrorRate)
			require.Equal(t, "unknown", health.TTFT)
			require.Equal(t, "unknown", health.Cache)
			require.Nil(t, health.Score)
			require.Nil(t, health.ErrorRateScore)
			require.Nil(t, health.TTFTScore)
			require.Nil(t, health.CacheScore)
		})
	}
}

func TestChannelMonitorV2HealthPrismLowSampleCacheRegression(t *testing.T) {
	// Production 2026-10-03 16:25: five successful probes, 195 input/cache
	// tokens, no TTFT. The old token-sample gate made cache the sole score.
	health := ChannelMonitorV2HealthForWithThresholds(ChannelMonitorV2Metric{
		RequestCount: 5, SuccessRequests: 5, ProbeRequestCount: 5,
		ErrorRate: 0, CacheRate: 0, CacheRateDenominator: 195,
	}, cacheSensitiveHealthThresholds())
	require.Equal(t, "unknown", health.Overall)
	require.Nil(t, health.Score)
	require.Equal(t, "unknown", health.Cache)
	require.Nil(t, health.CacheScore)
}

func TestChannelMonitorV2HealthZeroCacheDoesNotDegradeSuccessfulAPI(t *testing.T) {
	thresholds := cacheSensitiveHealthThresholds()
	for _, denominator := range []int64{0, 1, 195} {
		t.Run(strconv.FormatInt(denominator, 10), func(t *testing.T) {
			metrics := ChannelMonitorV2Metric{
				RequestCount: 50, SuccessRequests: 50, ErrorRate: 0,
				CacheRate: 0, CacheRateDenominator: denominator,
			}
			health := ChannelMonitorV2HealthForWithThresholds(metrics, thresholds)
			require.Equal(t, "healthy", health.Overall)
			require.Equal(t, "healthy", health.ErrorRate)
			require.NotNil(t, health.Score)
			require.InDelta(t, 100.0, *health.Score, 0.001)
			require.Equal(t, "unknown", health.TTFT)
			require.Nil(t, health.TTFTScore)
			if denominator == 0 {
				require.Equal(t, "unknown", health.Cache)
				require.Nil(t, health.CacheScore)
			} else {
				require.Equal(t, "critical", health.Cache)
				require.NotNil(t, health.CacheScore)
				require.Zero(t, *health.CacheScore)
			}
		})
	}
}

func TestChannelMonitorV2HealthKeepsMeasuredTTFTSampleGate(t *testing.T) {
	p50 := int64(30000)
	metrics := ChannelMonitorV2Metric{
		RequestCount: 50, ErrorRate: 0, CacheRate: 1, CacheRateDenominator: 50,
		TTFT: ChannelMonitorV2Latency{SampleCount: 49, P50Ms: &p50},
	}
	health := ChannelMonitorV2HealthForWithThresholds(metrics, cacheSensitiveHealthThresholds())
	require.Equal(t, "unknown", health.TTFT)
	require.Nil(t, health.TTFTScore)
	require.Equal(t, "healthy", health.Overall)
	require.NotNil(t, health.Score)
	require.InDelta(t, 100.0, *health.Score, 0.001)

	metrics.TTFT.SampleCount = 50
	health = ChannelMonitorV2HealthForWithThresholds(metrics, cacheSensitiveHealthThresholds())
	require.Equal(t, "critical", health.TTFT)
	require.NotNil(t, health.TTFTScore)
	require.Zero(t, *health.TTFTScore)
	require.Equal(t, "warning", health.Overall)
	require.NotNil(t, health.Score)
	require.InDelta(t, 75.0, *health.Score, 0.001)
}

func TestChannelMonitorV2HealthLegacyCacheOnlyWeightsUseAPIDefaults(t *testing.T) {
	thresholds := cacheSensitiveHealthThresholds()
	thresholds.ErrorWeight, thresholds.TTFTWeight, thresholds.CacheWeight = 0, 0, 1
	original := thresholds
	p50 := int64(30000)
	for _, test := range []struct {
		name      string
		errorRate float64
		ttft      ChannelMonitorV2Latency
		overall   string
		score     float64
	}{
		{name: "success", overall: "healthy", score: 100},
		{name: "API failure", errorRate: 1, overall: "critical", score: 0},
		{name: "slow TTFT", ttft: ChannelMonitorV2Latency{SampleCount: 50, P50Ms: &p50}, overall: "warning", score: 75},
	} {
		t.Run(test.name, func(t *testing.T) {
			health := ChannelMonitorV2HealthForWithThresholds(ChannelMonitorV2Metric{
				RequestCount: 50, ErrorRate: test.errorRate, TTFT: test.ttft,
				CacheRate: 0, CacheRateDenominator: 195,
			}, thresholds)
			require.Equal(t, test.overall, health.Overall)
			require.NotNil(t, health.Score)
			require.InDelta(t, test.score, *health.Score, 0.001)
			require.Equal(t, "critical", health.Cache)
			require.Equal(t, original, health.Thresholds)
			require.Equal(t, original, thresholds)
		})
	}
}

func TestChannelMonitorV2HealthRespectsDisabledAPISignalWeights(t *testing.T) {
	thresholds := cacheSensitiveHealthThresholds()
	thresholds.ErrorWeight, thresholds.TTFTWeight, thresholds.CacheWeight = 0, 1, 100
	metrics := ChannelMonitorV2Metric{RequestCount: 50, ErrorRate: 0, CacheRate: 1, CacheRateDenominator: 195}
	health := ChannelMonitorV2HealthForWithThresholds(metrics, thresholds)
	require.Equal(t, "healthy", health.ErrorRate)
	require.Equal(t, "healthy", health.Cache)
	require.Equal(t, "unknown", health.Overall)
	require.Nil(t, health.Score)

	p50 := int64(30000)
	metrics.TTFT = ChannelMonitorV2Latency{SampleCount: 50, P50Ms: &p50}
	health = ChannelMonitorV2HealthForWithThresholds(metrics, thresholds)
	require.Equal(t, "critical", health.TTFT)
	require.Equal(t, "critical", health.Overall)
	require.NotNil(t, health.Score)
	require.Zero(t, *health.Score)

	thresholds.ErrorWeight, thresholds.TTFTWeight = 1, 0
	metrics.ErrorRate = 1
	health = ChannelMonitorV2HealthForWithThresholds(metrics, thresholds)
	require.Equal(t, "critical", health.ErrorRate)
	require.Equal(t, "critical", health.Overall)
	require.NotNil(t, health.Score)
	require.Zero(t, *health.Score)
}

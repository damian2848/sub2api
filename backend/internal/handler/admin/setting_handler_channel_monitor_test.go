package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSettingsChannelMonitorHybridRoundTripAndOmission(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{
		service.SettingKeyChannelMonitorEnabled:                "true",
		service.SettingKeyChannelMonitorMode:                   service.ChannelMonitorModeV2,
		service.SettingKeyChannelMonitorDefaultIntervalSeconds: "120",
		service.SettingKeyChannelMonitorHideThroughput:         "true",
		service.SettingKeyChannelMonitorShowQuota:              "false",
		service.SettingKeyChannelMonitorHideUserRanking:        "true",
	})
	before := h.settingService.GetChannelMonitorRuntime(context.Background())
	require.False(t, before.ActiveProbesAllowed())
	require.True(t, before.PassiveAggregationAllowed())

	rec := doUpdateSettings(t, h, map[string]any{"channel_monitor_mode": " Hybrid "}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, service.ChannelMonitorModeHybrid, gjson.Get(rec.Body.String(), "data.channel_monitor_mode").String())
	require.Equal(t, service.ChannelMonitorModeHybrid, repo.values[service.SettingKeyChannelMonitorMode])
	runtime := h.settingService.GetChannelMonitorRuntime(context.Background())
	require.True(t, runtime.ActiveProbesAllowed())
	require.True(t, runtime.PassiveAggregationAllowed())
	require.True(t, runtime.QualityProbesAllowed())
	require.Equal(t, before.DefaultIntervalSeconds, runtime.DefaultIntervalSeconds)
	require.Equal(t, before.HideThroughput, runtime.HideThroughput)
	require.Equal(t, before.ShowQuota, runtime.ShowQuota)
	require.Equal(t, before.HideUserRanking, runtime.HideUserRanking)

	rec = doUpdateSettings(t, h, map[string]any{"site_name": "monitor settings retained"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, runtime, h.settingService.GetChannelMonitorRuntime(context.Background()))
	public, err := h.settingService.GetPublicSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, service.ChannelMonitorModeHybrid, public.ChannelMonitorMode)

	rec = doUpdateSettings(t, h, map[string]any{"channel_monitor_enabled": false}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	disabled := h.settingService.GetChannelMonitorRuntime(context.Background())
	require.Equal(t, service.ChannelMonitorModeHybrid, disabled.Mode)
	require.False(t, disabled.ActiveProbesAllowed())
	require.False(t, disabled.PassiveAggregationAllowed())
	require.False(t, disabled.QualityProbesAllowed())
}

func TestSettingsChannelMonitorLegacyModePreservedWhenOmitted(t *testing.T) {
	for _, mode := range []string{service.ChannelMonitorModeV1, service.ChannelMonitorModeV2} {
		t.Run(mode, func(t *testing.T) {
			h, repo := newStepUpSwitchTestHandler(t, map[string]string{
				service.SettingKeyChannelMonitorEnabled: "true",
				service.SettingKeyChannelMonitorMode:    mode,
			})
			rec := doUpdateSettings(t, h, map[string]any{"site_name": "unified monitor"}, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, mode, repo.values[service.SettingKeyChannelMonitorMode])
			runtime := h.settingService.GetChannelMonitorRuntime(context.Background())
			require.Equal(t, mode == service.ChannelMonitorModeV1, runtime.ActiveProbesAllowed())
			require.Equal(t, mode == service.ChannelMonitorModeV2, runtime.PassiveAggregationAllowed())
		})
	}
}

//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorRuntimeCollectionModes(t *testing.T) {
	tests := []struct {
		mode    string
		active  bool
		passive bool
		quality bool
	}{
		{ChannelMonitorModeV1, true, false, false},
		{ChannelMonitorModeV2, false, true, true},
		{ChannelMonitorModeHybrid, true, true, true},
		{"unknown", false, false, false},
		{"", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			runtime := ChannelMonitorRuntime{Enabled: true, Mode: tt.mode}
			require.Equal(t, tt.active, runtime.ActiveProbesAllowed())
			require.Equal(t, tt.passive, runtime.PassiveAggregationAllowed())
			require.Equal(t, tt.quality, runtime.QualityProbesAllowed())
			runtime.Enabled = false
			require.False(t, runtime.ActiveProbesAllowed())
			require.False(t, runtime.PassiveAggregationAllowed())
			require.False(t, runtime.QualityProbesAllowed())
		})
	}
}

func TestSettingServiceChannelMonitorModeNormalization(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"v1", ChannelMonitorModeV1},
		{"v2", ChannelMonitorModeV2},
		{"hybrid", ChannelMonitorModeHybrid},
		{" HYBRID ", ChannelMonitorModeHybrid},
		{" V2 ", ChannelMonitorModeV2},
		{"", ChannelMonitorModeV1},
		{"unknown", ChannelMonitorModeV1},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			svc := NewSettingService(&settingPublicRepoStub{values: map[string]string{
				SettingKeyChannelMonitorEnabled: "true",
				SettingKeyChannelMonitorMode:    tt.raw,
			}}, &config.Config{})
			require.Equal(t, tt.want, svc.GetChannelMonitorRuntime(context.Background()).Mode)
			public, err := svc.GetPublicSettings(context.Background())
			require.NoError(t, err)
			require.Equal(t, tt.want, public.ChannelMonitorMode)
			parsed := svc.parseSettings(map[string]string{SettingKeyChannelMonitorMode: tt.raw})
			require.Equal(t, tt.want, parsed.ChannelMonitorMode)
			updates, err := svc.buildSystemSettingsUpdates(context.Background(), &SystemSettings{ChannelMonitorMode: tt.raw})
			require.NoError(t, err)
			require.Equal(t, tt.want, updates[SettingKeyChannelMonitorMode])
		})
	}
}

func TestSettingServiceChannelMonitorRuntimeUnavailable(t *testing.T) {
	for _, svc := range []*SettingService{
		nil,
		NewSettingService(&settingPublicRepoStub{err: errors.New("settings unavailable")}, &config.Config{}),
	} {
		runtime := svc.GetChannelMonitorRuntime(context.Background())
		require.False(t, runtime.ActiveProbesAllowed())
		require.False(t, runtime.PassiveAggregationAllowed())
		require.False(t, runtime.QualityProbesAllowed())
	}
}

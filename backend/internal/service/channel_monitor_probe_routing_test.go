//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type probeRoutingRepoStub struct {
	quotaModeRepoStub
	bindings map[string]*ChannelMonitorAPIKeyBinding
	keysRead []string
	readErr  error
	created  []*ChannelMonitor
}

func (r *probeRoutingRepoStub) GetMonitorGroup(_ context.Context, id int64) (*Group, error) {
	return &Group{ID: id, Name: "group", Platform: MonitorProviderOpenAI}, nil
}

func (r *probeRoutingRepoStub) GetMonitorAPIKeyBinding(_ context.Context, key string) (*ChannelMonitorAPIKeyBinding, error) {
	r.keysRead = append(r.keysRead, key)
	return r.bindings[key], r.readErr
}

func (r *probeRoutingRepoStub) Create(_ context.Context, m *ChannelMonitor) error {
	stored := *m
	r.created = append(r.created, &stored)
	return nil
}

type probeRoutingRuntimeStub struct {
	channelMonitorRuntimeStub
	origins []string
	err     error
}

func (s probeRoutingRuntimeStub) GetChannelMonitorProbeSiteOrigins(context.Context) ([]string, error) {
	return s.origins, s.err
}

func newProbeRoutingService(repo *probeRoutingRepoStub, origins ...string) *ChannelMonitorService {
	svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{})
	svc.SetRuntimeReader(probeRoutingRuntimeStub{
		channelMonitorRuntimeStub: channelMonitorRuntimeStub{rt: ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV1}},
		origins:                   origins,
	})
	return svc
}

func TestChannelMonitorProbeRouteValidatesEffectiveLocalKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		key      string
		groupID  *int64
		headers  map[string]string
		wantKey  string
		wantErr  error
	}{
		{"site match", "https://site.example/v1", "group-3-key", int64Ptr(3), nil, "group-3-key", nil},
		{"site mismatch", "https://site.example", "group-3-key", int64Ptr(24), nil, "group-3-key", ErrChannelMonitorProbeGroupMismatch},
		{"site unknown key", "https://site.example", "external-key", int64Ptr(3), nil, "external-key", ErrChannelMonitorProbeKeyUnavailable},
		{"site unbound key", "https://site.example", "unbound-key", int64Ptr(3), nil, "unbound-key", ErrChannelMonitorProbeGroupMismatch},
		{"site monitor unbound", "https://site.example", "group-3-key", nil, nil, "group-3-key", nil},
		{"external endpoint preserved", "https://remote.example", "external-key", int64Ptr(3), nil, "external-key", nil},
		{"unconfigured site alias local key", "https://alias.example", "group-3-key", int64Ptr(24), nil, "group-3-key", ErrChannelMonitorProbeGroupMismatch},
		{"lowercase bearer overrides default", "https://site.example", "group-3-key", int64Ptr(24), map[string]string{"authorization": "Bearer group-24-key"}, "group-24-key", nil},
		{"x-api-key cannot shadow bearer", "https://site.example", "group-3-key", int64Ptr(24), map[string]string{"x-api-key": "group-24-key"}, "group-3-key", ErrChannelMonitorProbeGroupMismatch},
		{"empty bearer enables x-api-key", "https://site.example", "group-3-key", int64Ptr(24), map[string]string{"authorization": "", "x-api-key": "group-24-key"}, "group-24-key", nil},
		{"bearer wire whitespace", "https://site.example", "group-3-key", int64Ptr(24), map[string]string{"authorization": "  Bearer group-24-key  "}, "group-24-key", nil},
		{"x-api-key wire whitespace", "https://site.example", "group-3-key", int64Ptr(24), map[string]string{"authorization": "", "x-api-key": "  group-24-key  "}, "group-24-key", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &probeRoutingRepoStub{bindings: map[string]*ChannelMonitorAPIKeyBinding{
				"group-3-key":  {APIKeyID: 11, GroupID: int64Ptr(3)},
				"group-24-key": {APIKeyID: 12, GroupID: int64Ptr(24)},
				"unbound-key":  {APIKeyID: 13},
			}}
			svc := newProbeRoutingService(repo, "https://SITE.example:443/base")
			err := svc.validateProbeRoute(context.Background(), &ChannelMonitor{
				Provider: MonitorProviderOpenAI, Endpoint: tc.endpoint,
				APIKey: tc.key, GroupID: tc.groupID, ExtraHeaders: tc.headers,
			})
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
				require.NotContains(t, err.Error(), tc.key)
			}
			require.Equal(t, []string{tc.wantKey}, repo.keysRead)
		})
	}
}

func TestChannelMonitorProbeAuthProviderPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		headers  map[string]string
		want     string
	}{
		{"anthropic lowercase x-api-key override", MonitorProviderAnthropic, map[string]string{"X-API-KEY": "overridden"}, "overridden"},
		{"anthropic bearer takes priority", MonitorProviderAnthropic, map[string]string{"authorization": "bearer bearer-key"}, "bearer-key"},
		{"anthropic non-ASCII leading whitespace is not a bearer scheme", MonitorProviderAnthropic, map[string]string{"authorization": "\u00a0Bearer bearer-key"}, "default-key"},
		{"gemini native header takes priority", MonitorProviderGemini, map[string]string{"Authorization": "Bearer bearer-key"}, "default-key"},
		{"gemini native override", MonitorProviderGemini, map[string]string{"X-GOOG-API-KEY": "native-key", "Authorization": "Bearer bearer-key"}, "native-key"},
		{"gemini empty native uses bearer", MonitorProviderGemini, map[string]string{"X-GOOG-API-KEY": "", "Authorization": "Bearer bearer-key"}, "bearer-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, effectiveChannelMonitorProbeKey(&ChannelMonitor{
				Provider: tc.provider, APIKey: "default-key", ExtraHeaders: tc.headers,
			}))
		})
	}
}

func TestMergeChannelMonitorHeadersCanonicalizesOverride(t *testing.T) {
	base := map[string]string{"Authorization": "Bearer default", "x-api-key": "default"}
	for range 100 {
		merged := mergeHeaders(base, &CheckOptions{ExtraHeaders: map[string]string{
			"Authorization": "Bearer uppercase", "authorization": "Bearer lowercase", "X-API-KEY": "overridden", "HOST": "forbidden",
		}})
		require.Len(t, merged, 2)
		require.Equal(t, "Bearer lowercase", merged["Authorization"])
		require.Equal(t, "overridden", merged["X-Api-Key"])
	}
	require.Equal(t, "Bearer default", base["Authorization"])
}

func TestChannelMonitorProbeCreateRejectsMismatchBeforePersistence(t *testing.T) {
	repo := &probeRoutingRepoStub{bindings: map[string]*ChannelMonitorAPIKeyBinding{
		"group-3-key": {APIKeyID: 11, GroupID: int64Ptr(3)},
	}}
	svc := newProbeRoutingService(repo, "https://8.8.8.8")
	_, err := svc.Create(context.Background(), ChannelMonitorCreateParams{
		Name: "mismatch", Provider: MonitorProviderOpenAI, Endpoint: "https://8.8.8.8",
		APIKey: "group-3-key", PrimaryModel: "test-model", GroupID: int64Ptr(24), IntervalSeconds: 60,
	})
	require.ErrorIs(t, err, ErrChannelMonitorProbeGroupMismatch)
	require.Empty(t, repo.created)
}

func TestChannelMonitorProbeUpdateRejectsRebindingAndUsesHeaderOverride(t *testing.T) {
	repo := &probeRoutingRepoStub{
		quotaModeRepoStub: quotaModeRepoStub{monitor: &ChannelMonitor{
			ID: 1, Provider: MonitorProviderOpenAI, Endpoint: "https://site.example",
			APIKey: "OLD:group-3-key", PrimaryModel: "test-model", GroupID: int64Ptr(3), IntervalSeconds: 60,
		}},
		bindings: map[string]*ChannelMonitorAPIKeyBinding{
			"group-3-key":  {APIKeyID: 11, GroupID: int64Ptr(3)},
			"group-24-key": {APIKeyID: 12, GroupID: int64Ptr(24)},
		},
	}
	svc := newProbeRoutingService(repo, "https://site.example")
	_, err := svc.Update(context.Background(), 1, ChannelMonitorUpdateParams{GroupID: int64Ptr(24)})
	require.ErrorIs(t, err, ErrChannelMonitorProbeGroupMismatch)
	require.Empty(t, repo.updated)

	headers := map[string]string{"authorization": "Bearer group-24-key"}
	updated, err := svc.Update(context.Background(), 1, ChannelMonitorUpdateParams{GroupID: int64Ptr(24), ExtraHeaders: &headers})
	require.NoError(t, err)
	require.Equal(t, int64(24), *updated.GroupID)
	require.Len(t, repo.updated, 1)
	require.Equal(t, "OLD:group-3-key", repo.updated[0].APIKey)
}

func TestChannelMonitorProbeRunRechecksBindingBeforeNetwork(t *testing.T) {
	swapMonitorHTTPClient(t)
	requests := 0
	handler := &openAICaptureHandler{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	repo := &probeRoutingRepoStub{
		quotaModeRepoStub: quotaModeRepoStub{monitor: &ChannelMonitor{
			ID: 1, Name: "site-probe", Provider: MonitorProviderOpenAI,
			Endpoint: server.URL, APIKey: "OLD:group-3-key", PrimaryModel: "test-model", GroupID: int64Ptr(3),
		}},
		bindings: map[string]*ChannelMonitorAPIKeyBinding{"group-3-key": {APIKeyID: 11, GroupID: int64Ptr(3)}},
	}
	svc := newProbeRoutingService(repo, server.URL)
	results, err := svc.RunCheck(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, MonitorStatusOperational, results[0].Status)
	require.Equal(t, 1, requests)
	require.Len(t, repo.history, 1)

	repo.bindings["group-3-key"].GroupID = int64Ptr(24)
	results, err = svc.RunCheck(context.Background(), 1)
	require.ErrorIs(t, err, ErrChannelMonitorProbeGroupMismatch)
	require.Nil(t, results)
	require.Equal(t, 1, requests)
	require.Len(t, repo.history, 1, "a skipped mismatched execution is not a real generation request")
	require.Equal(t, []string{"group-3-key", "group-3-key"}, repo.keysRead)
}

func TestChannelMonitorProbeRoutingFailsClosedAndDoesNotLeakLookupError(t *testing.T) {
	repo := &probeRoutingRepoStub{readErr: errors.New("driver failed for secret-value")}
	svc := newProbeRoutingService(repo, "https://site.example")
	err := svc.validateProbeRoute(context.Background(), &ChannelMonitor{
		Provider: MonitorProviderOpenAI, Endpoint: "https://site.example", APIKey: "secret-value", GroupID: int64Ptr(3),
	})
	require.ErrorIs(t, err, ErrChannelMonitorProbeRouteUnavailable)
	require.NotContains(t, err.Error(), "secret-value")

	svc = NewChannelMonitorService(&quotaModeRepoStub{}, nil)
	svc.SetRuntimeReader(probeRoutingRuntimeStub{origins: []string{"https://site.example"}})
	err = svc.validateProbeRoute(context.Background(), &ChannelMonitor{Endpoint: "https://site.example", Provider: MonitorProviderOpenAI})
	require.ErrorIs(t, err, ErrChannelMonitorProbeRouteUnavailable)
}

func TestChannelMonitorProbeQuotaModeDoesNotLookUpCredentials(t *testing.T) {
	repo := &probeRoutingRepoStub{readErr: errors.New("unexpected lookup")}
	svc := newProbeRoutingService(repo, "https://site.example")
	require.NoError(t, svc.validateProbeRoute(context.Background(), &ChannelMonitor{CheckMode: MonitorCheckModeQuota}))
	require.Empty(t, repo.keysRead)
}

type probeOriginsSettingRepoStub struct {
	SettingRepository
	value string
	err   error
}

func (s probeOriginsSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if key != SettingKeyAPIBaseURL {
		return "", errors.New("unexpected setting")
	}
	return s.value, s.err
}

func TestSettingServiceChannelMonitorProbeOrigins(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.FrontendURL = "https://site.example"
	svc := NewSettingService(probeOriginsSettingRepoStub{value: "https://api.site.example/v1"}, cfg)
	origins, err := svc.GetChannelMonitorProbeSiteOrigins(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"https://site.example", "https://api.site.example/v1"}, origins)

	svc.settingRepo = probeOriginsSettingRepoStub{err: ErrSettingNotFound}
	origins, err = svc.GetChannelMonitorProbeSiteOrigins(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"https://site.example"}, origins)

	svc.settingRepo = probeOriginsSettingRepoStub{err: errors.New("database unavailable")}
	_, err = svc.GetChannelMonitorProbeSiteOrigins(context.Background())
	require.ErrorIs(t, err, ErrChannelMonitorProbeRouteUnavailable)
}

func TestChannelMonitorSameOrigin(t *testing.T) {
	require.True(t, sameChannelMonitorOrigin("https://SITE.example:443/v1", "https://site.example/ui"))
	require.False(t, sameChannelMonitorOrigin("https://site.example:8443", "https://site.example"))
	require.False(t, sameChannelMonitorOrigin("http://site.example", "https://site.example"))
	require.False(t, sameChannelMonitorOrigin("https://external.example", "https://site.example"))
}

func TestChannelMonitorProbeDisableOnlyAllowsBrokenLegacyRouting(t *testing.T) {
	repo := &probeRoutingRepoStub{
		quotaModeRepoStub: quotaModeRepoStub{monitor: &ChannelMonitor{
			ID: 1, Provider: MonitorProviderOpenAI, Endpoint: "https://site.example",
			APIKey: "OLD:group-3-key", PrimaryModel: "test-model", GroupID: int64Ptr(24), IntervalSeconds: 60, Enabled: true,
		}},
		bindings: map[string]*ChannelMonitorAPIKeyBinding{"group-3-key": {APIKeyID: 11, GroupID: int64Ptr(3)}},
	}
	svc := newProbeRoutingService(repo, "https://site.example")
	disabled := false
	monitor, err := svc.Update(context.Background(), 1, ChannelMonitorUpdateParams{Enabled: &disabled})
	require.NoError(t, err)
	require.False(t, monitor.Enabled)
	require.Len(t, repo.updated, 1)
	require.Empty(t, repo.keysRead, "stopping cannot require valid routing")

	name := "also changed"
	_, err = svc.Update(context.Background(), 1, ChannelMonitorUpdateParams{Enabled: &disabled, Name: &name})
	require.ErrorIs(t, err, ErrChannelMonitorProbeGroupMismatch)
	require.Len(t, repo.updated, 1)
}

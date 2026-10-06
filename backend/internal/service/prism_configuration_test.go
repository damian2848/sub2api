package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func prismConfigurationTestOptions() PrismConfigurationOptions {
	return PrismConfigurationOptions{PrewarmChat: true, StreamReasoning: true, MemoryReserveMiB: 32}
}

func prismConfigurationTestState() PrismConfigurationState {
	options := prismConfigurationTestOptions()
	return PrismConfigurationState{Effective: options, Desired: options, Source: "environment", ApplyMode: "restart"}
}

func prismConfigurationTestService(t *testing.T, endpoint string) *PrismAccountService {
	t.Helper()
	svc := NewPrismAccountService(nil, nil, &prismTestTokens{}, PrismRuntimeConfig{
		URL: endpoint, ManagementKey: "private-prism-management-key-canary",
	})
	t.Cleanup(svc.Stop)
	return svc
}

type prismConfigurationSettingsRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func (r *prismConfigurationSettingsRepo) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}

func (r *prismConfigurationSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *prismConfigurationSettingsRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}

func (r *prismConfigurationSettingsRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, nil
}

func (r *prismConfigurationSettingsRepo) SetMultiple(context.Context, map[string]string) error {
	return nil
}

func (r *prismConfigurationSettingsRepo) GetAll(context.Context) (map[string]string, error) {
	return nil, nil
}

func (r *prismConfigurationSettingsRepo) Delete(context.Context, string) error {
	return nil
}

func TestPrismConfigurationStrictCompleteOptions(t *testing.T) {
	valid, err := json.Marshal(prismConfigurationTestOptions())
	require.NoError(t, err)
	decoded, err := DecodePrismConfigurationOptions(strings.NewReader(string(valid)))
	require.NoError(t, err)
	require.Equal(t, prismConfigurationTestOptions(), decoded)
	for name, body := range map[string]string{
		"missing":       `{}`,
		"null":          strings.Replace(string(valid), `"http_cache":false`, `"http_cache":null`, 1),
		"wrong_bool":    strings.Replace(string(valid), `"http_cache":false`, `"http_cache":"false"`, 1),
		"wrong_int":     strings.Replace(string(valid), `"memory_limit_mib":0`, `"memory_limit_mib":1.0`, 1),
		"negative":      strings.Replace(string(valid), `"memory_limit_mib":0`, `"memory_limit_mib":-1`, 1),
		"huge":          strings.Replace(string(valid), `"memory_limit_mib":0`, `"memory_limit_mib":1048577`, 1),
		"huge_reserve":  strings.Replace(string(valid), `"memory_reserve_mib":32`, `"memory_reserve_mib":1048577`, 1),
		"equal_memory":  strings.Replace(string(valid), `"memory_limit_mib":0`, `"memory_limit_mib":32`, 1),
		"small_memory":  strings.Replace(string(valid), `"memory_limit_mib":0`, `"memory_limit_mib":1`, 1),
		"case_variant":  strings.Replace(string(valid), `"http_cache"`, `"HTTP_CACHE"`, 1),
		"duplicate":     strings.TrimSuffix(string(valid), "}") + `,"http_cache":true}`,
		"url_injection": strings.TrimSuffix(string(valid), "}") + `,"base_url":"http://canary.invalid"}`,
		"key_injection": strings.TrimSuffix(string(valid), "}") + `,"management_key":"private-secret-canary"}`,
		"trailing":      string(valid) + `{}`,
		"oversized":     string(valid) + strings.Repeat(" ", PrismConfigurationRequestLimit),
		"array":         `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodePrismConfigurationOptions(strings.NewReader(body))
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
			require.Equal(t, "PRISM_SETTINGS_INVALID", infraerrors.Reason(err))
			require.NotContains(t, err.Error(), "canary")
		})
	}
	for _, options := range []PrismConfigurationOptions{
		{}, {MemoryLimitMiB: 1048576, MemoryReserveMiB: 1048575}, {MemoryReserveMiB: 1048576},
	} {
		body, err := json.Marshal(options)
		require.NoError(t, err)
		got, err := DecodePrismConfigurationOptions(strings.NewReader(string(body)))
		require.NoError(t, err)
		require.Equal(t, options, got)
	}
}

func TestPrismConfigurationFixedConnectionAndPendingRestart(t *testing.T) {
	state := prismConfigurationTestState()
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/config", r.URL.Path)
		require.Empty(t, r.URL.RawQuery)
		require.Equal(t, "Bearer private-prism-management-key-canary", r.Header.Get("Authorization"))
		methods = append(methods, r.Method)
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			options, err := decodePrismConfigurationOptions(body)
			require.NoError(t, err)
			state.Desired, state.Source = options, "saved"
			state.RestartRequired = state.Effective != state.Desired
		case http.MethodDelete:
			state.Desired, state.Source, state.RestartRequired = state.Effective, "environment", false
		}
		require.NoError(t, json.NewEncoder(w).Encode(state))
	}))
	defer server.Close()
	svc := prismConfigurationTestService(t, server.URL)
	routing := svc.cfg
	result, err := svc.GetConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, "ready", result.Availability)
	require.Equal(t, state, *result.Configuration)
	require.Equal(t, PrismGatewayConfiguration{Enabled: true, Configured: true, BaseURL: server.URL, ManagementKeyConfigured: true}, result.Gateway)

	opts := prismConfigurationTestOptions()
	opts.ProjectIsolation = true
	result, err = svc.UpdateConfiguration(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, "ready", result.Availability)
	require.Equal(t, opts, result.Configuration.Desired)
	require.Equal(t, prismConfigurationTestOptions(), result.Configuration.Effective)
	require.True(t, result.Configuration.RestartRequired)
	require.Equal(t, "saved", result.Configuration.Source)
	require.Equal(t, routing, svc.cfg, "saving startup options cannot change gateway routing")

	result, err = svc.ResetConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, "ready", result.Availability)
	require.False(t, result.Configuration.RestartRequired)
	require.Equal(t, []string{http.MethodGet, http.MethodPut, http.MethodDelete}, methods)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-prism-management-key-canary")
}

func TestPrismConfigurationResetPersistsSidecarDesiredEnabled(t *testing.T) {
	// The backend process environment intentionally disagrees with the sidecar's
	// environment. Reset must persist the state returned by the sidecar rather
	// than deriving a value from this process's environment.
	t.Setenv("PRISM_BROWSER_ENABLED", "false")
	state := prismConfigurationTestState()
	state.Effective.Enabled = true
	state.Desired.Enabled = true
	repo := &prismConfigurationSettingsRepo{values: make(map[string]string)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method)
		state.Effective.Enabled = true
		state.Desired.Enabled = true
		state.RestartRequired = false
		state.Source = "environment"
		require.NoError(t, json.NewEncoder(w).Encode(state))
	}))
	defer server.Close()
	svc := NewPrismAccountService(nil, nil, &prismTestTokens{}, PrismRuntimeConfig{
		URL: server.URL, ManagementKey: "private-prism-management-key-canary",
	}, &SettingService{settingRepo: repo})
	t.Cleanup(svc.Stop)

	result, err := svc.ResetConfiguration(context.Background())
	require.NoError(t, err)
	require.True(t, result.Configuration.Desired.Enabled)
	require.True(t, result.Gateway.Enabled)
	require.Equal(t, "true", repo.values[SettingKeyPrismEnabled])
}

func TestPrismConfigurationRetainsStartupConnectionWhenRoutingDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(prismConfigurationTestState()))
	}))
	defer server.Close()
	t.Setenv("PRISM_BROWSER_BASE_URL", server.URL)
	t.Setenv("PRISM_MANAGEMENT_KEY", "private-prism-management-key-canary")
	t.Setenv("PRISM_BROWSER_ENABLED", "false")
	svc := ProvidePrismAccountService(nil, nil, nil, &config.Config{})
	t.Cleanup(svc.Stop)
	require.Empty(t, svc.cfg.URL, "existing gateway routing remains disabled")
	require.Equal(t, server.URL, svc.managementCfg.URL)
	result, err := svc.GetConfiguration(context.Background())
	require.NoError(t, err)
	require.False(t, result.Gateway.Enabled)
	require.True(t, result.Gateway.Configured)
	require.Equal(t, "ready", result.Availability)
}

func TestPrismConfigurationUnavailableReadMetadataAndSafeWriteErrors(t *testing.T) {
	valid, err := json.Marshal(prismConfigurationTestState())
	require.NoError(t, err)
	for _, test := range []struct {
		name, body, availability string
		status                   int
	}{
		{"unsupported", "private-upstream-token-canary", "unsupported", 404},
		{"unauthorized", "private-upstream-token-canary", "unavailable", 401},
		{"failure", "private-upstream-token-canary", "unavailable", 500},
		{"invalid_json", "private-upstream-token-canary", "unavailable", 200},
		{"oversized", string(valid) + strings.Repeat(" ", prismConfigurationResponseLimit), "unavailable", 200},
		{"missing_state", `{}`, "unavailable", 200},
		{"extra_secret", strings.TrimSuffix(string(valid), "}") + `,"management_key":"private-upstream-token-canary"}`, "unavailable", 200},
		{"invalid_mode", strings.Replace(string(valid), `"restart"`, `"immediate"`, 1), "unavailable", 200},
		{"invalid_source", strings.Replace(string(valid), `"environment"`, `"unknown"`, 1), "unavailable", 200},
		{"false_pending", strings.Replace(string(valid), `"restart_required":false`, `"restart_required":true`, 1), "unavailable", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			svc := prismConfigurationTestService(t, server.URL)
			result, err := svc.GetConfiguration(context.Background())
			require.NoError(t, err)
			require.Equal(t, test.availability, result.Availability)
			require.Nil(t, result.Configuration)
			_, err = svc.UpdateConfiguration(context.Background(), prismConfigurationTestOptions())
			require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
			require.NotContains(t, err.Error(), "canary")
			_, err = svc.ResetConfiguration(context.Background())
			require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
			require.NotContains(t, err.Error(), "canary")
		})
	}
	var missing *PrismAccountService
	result, err := missing.GetConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, "not_configured", result.Availability)
	require.Nil(t, result.Configuration)
	_, err = missing.ResetConfiguration(context.Background())
	require.Equal(t, "PRISM_SETTINGS_NOT_CONFIGURED", infraerrors.Reason(err))
}

type prismConfigurationRoundTripper func(*http.Request) (*http.Response, error)

func (transport prismConfigurationRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func TestPrismConfigurationTimeoutAndRedirectNeverLeakSecrets(t *testing.T) {
	svc := prismConfigurationTestService(t, "https://configured.invalid")
	svc.client.Transport = prismConfigurationRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		require.InDelta(t, 5, time.Until(deadline).Seconds(), 0.2)
		return nil, errors.New("https://private-address.invalid/private-token-canary")
	})
	result, err := svc.GetConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, "unavailable", result.Availability)
	_, err = svc.ResetConfiguration(context.Background())
	require.NotContains(t, err.Error(), "private-token-canary")

	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	svc = prismConfigurationTestService(t, redirect.URL)
	result, err = svc.GetConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, "unavailable", result.Availability)
	require.Zero(t, hits.Load())
}

func TestPrismConfigurationObserverDeniedBeforeManagementRequest(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	svc := prismConfigurationTestService(t, server.URL)
	ctx := WithObserverScope(context.Background(), []int64{10})
	_, err := svc.GetConfiguration(ctx)
	require.ErrorIs(t, err, ErrObserverScope)
	_, err = svc.UpdateConfiguration(ctx, prismConfigurationTestOptions())
	require.ErrorIs(t, err, ErrObserverScope)
	_, err = svc.ResetConfiguration(ctx)
	require.ErrorIs(t, err, ErrObserverScope)
	require.Zero(t, calls.Load())
}

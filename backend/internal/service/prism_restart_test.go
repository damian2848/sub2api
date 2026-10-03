package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

const prismRestartTestRuntimeID = "2f7de638-6240-4a81-91e3-639f4b853f32"

func prismRestartTestRequest() PrismRestartRequest {
	return PrismRestartRequest{ExpectedRuntimeID: prismRestartTestRuntimeID, ExpectedConfiguration: prismConfigurationTestOptions()}
}
func prismRestartTestBody(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return string(data)
}
func prismRestartTestRuntime(state string) PrismRestartRuntime {
	return PrismRestartRuntime{Supported: true, RuntimeID: prismRestartTestRuntimeID, State: state}
}

func TestPrismRestartStrictCompleteRequest(t *testing.T) {
	valid := prismRestartTestBody(t, prismRestartTestRequest())
	got, err := DecodePrismRestartRequest(strings.NewReader(valid))
	require.NoError(t, err)
	require.Equal(t, prismRestartTestRequest(), got)
	for name, body := range map[string]string{
		"missing":           `{}`,
		"missing_runtime":   `{"expected_configuration":` + prismRestartTestBody(t, prismConfigurationTestOptions()) + `}`,
		"missing_config":    `{"expected_runtime_id":"` + prismRestartTestRuntimeID + `"}`,
		"null_runtime":      strings.Replace(valid, `"`+prismRestartTestRuntimeID+`"`, `null`, 1),
		"null_config":       `{"expected_runtime_id":"` + prismRestartTestRuntimeID + `","expected_configuration":null}`,
		"wrong_runtime":     strings.Replace(valid, `"`+prismRestartTestRuntimeID+`"`, `123`, 1),
		"invalid_runtime":   strings.Replace(valid, prismRestartTestRuntimeID, "private-secret-canary", 1),
		"uuid_v1":           strings.Replace(valid, "-4a81-", "-1a81-", 1),
		"uuid_variant":      strings.Replace(valid, "-91e3-", "-71e3-", 1),
		"uuid_uppercase":    strings.Replace(valid, prismRestartTestRuntimeID, strings.ToUpper(prismRestartTestRuntimeID), 1),
		"uuid_newline":      strings.Replace(valid, prismRestartTestRuntimeID, prismRestartTestRuntimeID+`\n`, 1),
		"case_variant":      strings.Replace(valid, "expected_runtime_id", "Expected_Runtime_ID", 1),
		"duplicate_root":    strings.TrimSuffix(valid, "}") + `,"expected_runtime_id":"` + prismRestartTestRuntimeID + `"}`,
		"url_injection":     strings.TrimSuffix(valid, "}") + `,"base_url":"http://private-secret-canary.invalid"}`,
		"command_injection": strings.TrimSuffix(valid, "}") + `,"command":"private-command-secret-canary"}`,
		"key_injection":     strings.TrimSuffix(valid, "}") + `,"management_key":"private-key-secret-canary"}`,
		"missing_inner":     strings.Replace(valid, `"http_cache":false,`, "", 1),
		"case_inner":        strings.Replace(valid, `"http_cache"`, `"HTTP_CACHE"`, 1),
		"duplicate_inner":   strings.Replace(valid, `"http_cache":false`, `"http_cache":false,"http_cache":true`, 1),
		"unknown_inner":     strings.Replace(valid, `"http_cache":false`, `"http_cache":false,"path":"private-path-secret-canary"`, 1),
		"null_inner":        strings.Replace(valid, `"http_cache":false`, `"http_cache":null`, 1),
		"wrong_bool":        strings.Replace(valid, `"http_cache":false`, `"http_cache":"false"`, 1),
		"wrong_int":         strings.Replace(valid, `"memory_limit_mib":0`, `"memory_limit_mib":1.0`, 1),
		"negative":          strings.Replace(valid, `"memory_limit_mib":0`, `"memory_limit_mib":-1`, 1),
		"too_large":         strings.Replace(valid, `"memory_limit_mib":0`, `"memory_limit_mib":1048577`, 1),
		"invalid_memory":    strings.Replace(valid, `"memory_limit_mib":0`, `"memory_limit_mib":32`, 1),
		"array":             `[]`, "null": `null`, "trailing": valid + `{}`,
		"oversized": valid + strings.Repeat(" ", PrismRestartRequestLimit),
		"malformed": `{bad-private-secret-canary`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodePrismRestartRequest(strings.NewReader(body))
			require.Equal(t, PrismRestartRequest{}, got)
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
			require.Equal(t, "PRISM_RESTART_INVALID", infraerrors.Reason(err))
			require.NotContains(t, err.Error(), "canary")
		})
	}
	_, err = DecodePrismRestartRequest(nil)
	require.Equal(t, "PRISM_RESTART_INVALID", infraerrors.Reason(err))
}

func TestPrismRestartFixedManagementConnectionAndAcceptedBoot(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/restart", r.URL.Path)
		require.Empty(t, r.URL.RawQuery)
		require.Equal(t, "Bearer private-prism-management-key-canary", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		methods = append(methods, r.Method)
		runtime := prismRestartTestRuntime("ready")
		if r.Method == http.MethodPost {
			got, err := DecodePrismRestartRequest(r.Body)
			require.NoError(t, err)
			require.Equal(t, prismRestartTestRequest(), got)
			runtime.State = "restarting"
			w.WriteHeader(http.StatusAccepted)
		}
		require.NoError(t, json.NewEncoder(w).Encode(runtime))
	}))
	defer server.Close()
	svc := prismConfigurationTestService(t, server.URL)
	routing := svc.cfg
	result, err := svc.GetRestartStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, &PrismRestartResult{Availability: "ready", Runtime: new(prismRestartTestRuntime("ready"))}, result)
	result, err = svc.Restart(context.Background(), prismRestartTestRequest())
	require.NoError(t, err)
	require.Equal(t, &PrismRestartResult{Availability: "ready", Runtime: new(prismRestartTestRuntime("restarting"))}, result)
	require.Equal(t, []string{http.MethodGet, http.MethodPost}, methods, "POST must not retarget the reviewed boot through a new GET")
	require.Equal(t, routing, svc.cfg)
	require.NotContains(t, prismRestartTestBody(t, result), "canary")
}

func TestPrismRestartRetainsStartupConnectionWhenRoutingDisabled(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		runtime := prismRestartTestRuntime("ready")
		if r.Method == http.MethodPost {
			runtime.State = "restarting"
			w.WriteHeader(http.StatusAccepted)
		}
		require.Equal(t, "Bearer private-prism-management-key-canary", r.Header.Get("Authorization"))
		require.NoError(t, json.NewEncoder(w).Encode(runtime))
	}))
	defer server.Close()
	t.Setenv("PRISM_BROWSER_BASE_URL", server.URL)
	t.Setenv("PRISM_MANAGEMENT_KEY", "private-prism-management-key-canary")
	t.Setenv("PRISM_BROWSER_ENABLED", "false")
	svc := ProvidePrismAccountService(nil, nil, nil, &config.Config{})
	t.Cleanup(svc.Stop)
	require.Empty(t, svc.cfg.URL)
	require.Equal(t, server.URL, svc.managementCfg.URL)
	result, err := svc.GetRestartStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, "ready", result.Availability)
	result, err = svc.Restart(context.Background(), prismRestartTestRequest())
	require.NoError(t, err)
	require.Equal(t, "ready", result.Availability)
	require.Equal(t, int64(2), calls.Load())
	require.Empty(t, svc.cfg.URL)
}

func TestPrismRestartGracefulStatusAndSafeFailureErrors(t *testing.T) {
	ready := prismRestartTestBody(t, prismRestartTestRuntime("ready"))
	unsupported := `{"error":{"code":"prism_restart_unsupported","message":"private-upstream-secret-canary","type":"prism_error"}}`
	for _, test := range []struct {
		name, body, availability, reason string
		status                           int
	}{
		{"old_sidecar", "private-upstream-secret-canary", "unsupported", "PRISM_RESTART_UNSUPPORTED", 404},
		{"unsupported_error", unsupported, "unsupported", "PRISM_RESTART_UNSUPPORTED", 503},
		{"unauthorized", "private-upstream-secret-canary", "unavailable", "PRISM_RESTART_UNAVAILABLE", 401},
		{"failed", "private-upstream-secret-canary", "unavailable", "PRISM_RESTART_UNAVAILABLE", 500},
		{"other_503", strings.Replace(unsupported, "prism_restart_unsupported", "request_timeout", 1), "unavailable", "PRISM_RESTART_UNAVAILABLE", 503},
		{"invalid_json", "private-upstream-secret-canary", "unavailable", "PRISM_RESTART_UNAVAILABLE", 200},
		{"oversized", ready + strings.Repeat(" ", prismRestartResponseLimit), "unavailable", "PRISM_RESTART_UNAVAILABLE", 200},
		{"missing", `{}`, "unavailable", "PRISM_RESTART_UNAVAILABLE", 200},
		{"null", `null`, "unavailable", "PRISM_RESTART_UNAVAILABLE", 200},
		{"extra_secret", strings.TrimSuffix(ready, "}") + `,"management_key":"private-upstream-secret-canary"}`, "unavailable", "PRISM_RESTART_UNAVAILABLE", 200},
		{"null_state", strings.Replace(ready, `"ready"`, `null`, 1), "unavailable", "PRISM_RESTART_UNAVAILABLE", 200},
		{"invalid_state", strings.Replace(ready, `"ready"`, `"private-upstream-secret-canary"`, 1), "unavailable", "PRISM_RESTART_UNAVAILABLE", 200},
		{"invalid_uuid", strings.Replace(ready, prismRestartTestRuntimeID, "private-upstream-secret-canary", 1), "unavailable", "PRISM_RESTART_UNAVAILABLE", 200},
		{"wrong_supported", strings.Replace(ready, `true`, `"true"`, 1), "unavailable", "PRISM_RESTART_UNAVAILABLE", 200},
		{"duplicate", strings.TrimSuffix(ready, "}") + `,"supported":true}`, "unavailable", "PRISM_RESTART_UNAVAILABLE", 200},
		{"wrong_200_for_post", ready, "ready", "PRISM_RESTART_UNAVAILABLE", 200},
		{"unsupported_capability", strings.Replace(ready, `true`, `false`, 1), "unsupported", "PRISM_RESTART_UNAVAILABLE", 200},
		{"accepted_wrong_boot", strings.Replace(strings.Replace(ready, prismRestartTestRuntimeID, "06bf60b6-988a-46f0-8abe-1c80110935b8", 1), `"ready"`, `"restarting"`, 1), "unavailable", "PRISM_RESTART_UNAVAILABLE", 202},
		{"accepted_not_restarting", ready, "unavailable", "PRISM_RESTART_UNAVAILABLE", 202},
		{"accepted_unsupported", strings.Replace(strings.Replace(ready, `true`, `false`, 1), `"ready"`, `"restarting"`, 1), "unavailable", "PRISM_RESTART_UNAVAILABLE", 202},
		{"conflict", "private-upstream-secret-canary", "unavailable", "PRISM_RESTART_CONFLICT", 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			svc := prismConfigurationTestService(t, server.URL)
			result, err := svc.GetRestartStatus(context.Background())
			require.NoError(t, err)
			require.Equal(t, test.availability, result.Availability)
			if result.Availability == "ready" {
				require.NotNil(t, result.Runtime)
				require.True(t, result.Runtime.Supported)
			} else {
				require.Nil(t, result.Runtime)
			}
			require.NotContains(t, prismRestartTestBody(t, result), "canary")
			result, err = svc.Restart(context.Background(), prismRestartTestRequest())
			require.Nil(t, result)
			if test.status == 409 {
				require.Equal(t, http.StatusConflict, infraerrors.Code(err))
			} else {
				require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
			}
			require.Equal(t, test.reason, infraerrors.Reason(err))
			require.NotContains(t, err.Error(), "canary")
			require.NotContains(t, err.Error(), server.URL)
		})
	}
}

func TestPrismRestartUnknownSupportAndNotConfigured(t *testing.T) {
	for _, svc := range []*PrismAccountService{
		nil, prismConfigurationTestService(t, ""),
		NewPrismAccountService(nil, nil, nil, PrismRuntimeConfig{URL: "http://configured.invalid"}),
		prismConfigurationTestService(t, "http://configured.invalid/injected-path-canary"),
	} {
		if svc != nil {
			defer svc.Stop()
		}
		result, err := svc.GetRestartStatus(context.Background())
		require.NoError(t, err)
		require.Equal(t, &PrismRestartResult{Availability: "not_configured"}, result)
		result, err = svc.Restart(context.Background(), prismRestartTestRequest())
		require.Nil(t, result)
		require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
		require.Equal(t, "PRISM_RESTART_NOT_CONFIGURED", infraerrors.Reason(err))
		require.NotContains(t, err.Error(), "canary")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(prismRestartTestRuntime("restarting")))
	}))
	defer server.Close()
	result, err := prismConfigurationTestService(t, server.URL).GetRestartStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, "ready", result.Availability)
	require.Equal(t, "restarting", result.Runtime.State)
}

func TestPrismRestartObserverAndInvalidRequestDeniedBeforeManagementRequest(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	svc := prismConfigurationTestService(t, server.URL)
	for _, groups := range [][]int64{{10}, {}} {
		ctx := WithObserverScope(context.Background(), groups)
		_, err := svc.GetRestartStatus(ctx)
		require.ErrorIs(t, err, ErrObserverScope)
		_, err = svc.Restart(ctx, prismRestartTestRequest())
		require.ErrorIs(t, err, ErrObserverScope)
		_, err = svc.Restart(ctx, PrismRestartRequest{})
		require.ErrorIs(t, err, ErrObserverScope, "observer denial must precede input validation")
		var missing *PrismAccountService
		_, err = missing.GetRestartStatus(ctx)
		require.ErrorIs(t, err, ErrObserverScope, "observer denial must precede connection availability")
	}
	for _, request := range []PrismRestartRequest{{}, {ExpectedRuntimeID: prismRestartTestRuntimeID, ExpectedConfiguration: PrismConfigurationOptions{MemoryLimitMiB: -1}}} {
		_, err := svc.Restart(context.Background(), request)
		require.Equal(t, "PRISM_RESTART_INVALID", infraerrors.Reason(err))
	}
	require.Zero(t, calls.Load())
}

func TestPrismRestartSavedConfigurationAndBootConflict(t *testing.T) {
	request := prismRestartTestRequest()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, http.MethodPost, r.Method)
		got, err := DecodePrismRestartRequest(r.Body)
		require.NoError(t, err)
		require.NotEqual(t, request, got)
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"prism_restart_conflict","message":"private-conflict-secret-canary"}}`)
	}))
	defer server.Close()
	svc := prismConfigurationTestService(t, server.URL)
	for _, mutate := range []func(*PrismRestartRequest){
		func(request *PrismRestartRequest) { request.ExpectedRuntimeID = "06bf60b6-988a-46f0-8abe-1c80110935b8" },
		func(request *PrismRestartRequest) { request.ExpectedConfiguration.HTTPCache = true },
	} {
		stale := request
		mutate(&stale)
		result, err := svc.Restart(context.Background(), stale)
		require.Nil(t, result)
		require.Equal(t, http.StatusConflict, infraerrors.Code(err))
		require.Equal(t, "PRISM_RESTART_CONFLICT", infraerrors.Reason(err))
		require.NotContains(t, err.Error(), "canary")
	}
	require.Equal(t, int64(2), calls.Load())
}

func TestPrismRestartTimeoutAndRedirectNeverLeakSecrets(t *testing.T) {
	svc := prismConfigurationTestService(t, "https://configured.invalid")
	svc.client.Transport = prismConfigurationRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		require.InDelta(t, 5, time.Until(deadline).Seconds(), 0.2)
		return nil, errors.New("https://private-upstream.invalid/private-token-canary")
	})
	result, err := svc.GetRestartStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, &PrismRestartResult{Availability: "unavailable"}, result)
	_, err = svc.Restart(context.Background(), prismRestartTestRequest())
	require.Equal(t, "PRISM_RESTART_UNAVAILABLE", infraerrors.Reason(err))
	require.NotContains(t, err.Error(), "canary")
	svc.client.Transport = prismConfigurationRoundTripper(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = svc.Restart(ctx, prismRestartTestRequest())
	require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
	require.Equal(t, "PRISM_RESTART_UNAVAILABLE", infraerrors.Reason(err))
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/private-token-canary", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	svc = prismConfigurationTestService(t, redirect.URL)
	svc.client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	result, err = svc.GetRestartStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, &PrismRestartResult{Availability: "unavailable"}, result)
	_, err = svc.Restart(context.Background(), prismRestartTestRequest())
	require.Equal(t, "PRISM_RESTART_UNAVAILABLE", infraerrors.Reason(err))
	require.NotContains(t, err.Error(), "canary")
	require.Zero(t, hits.Load(), "neither GET nor POST may follow a management redirect")
}

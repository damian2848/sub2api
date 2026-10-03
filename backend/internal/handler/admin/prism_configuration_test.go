package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func prismConfigurationHandlerOptions() service.PrismConfigurationOptions {
	return service.PrismConfigurationOptions{PrewarmChat: true, StreamReasoning: true, MemoryReserveMiB: 32}
}

func prismConfigurationHandlerState() service.PrismConfigurationState {
	options := prismConfigurationHandlerOptions()
	return service.PrismConfigurationState{Effective: options, Desired: options, Source: "environment", ApplyMode: "restart"}
}

func prismConfigurationHandlerRouter(h *PrismHandler, observer bool, routeGuard bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if observer {
		r.Use(func(c *gin.Context) {
			c.Request = c.Request.WithContext(service.WithObserverScope(c.Request.Context(), []int64{10}))
		})
	}
	if routeGuard {
		r.Use((&AccountHandler{}).AuthorizeObserver)
	}
	r.GET("/api/v1/admin/settings/prism", h.GetConfiguration)
	r.PUT("/api/v1/admin/settings/prism", h.UpdateConfiguration)
	r.DELETE("/api/v1/admin/settings/prism", h.ResetConfiguration)
	return r
}

func TestPrismConfigurationHandlerCompleteEnvelopeForAllMethods(t *testing.T) {
	state := prismConfigurationHandlerState()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/config", r.URL.Path)
		require.Equal(t, "Bearer handler-management-secret-canary", r.Header.Get("Authorization"))
		require.NoError(t, json.NewEncoder(w).Encode(state))
	}))
	defer server.Close()
	svc := service.NewPrismAccountService(nil, nil, nil, service.PrismRuntimeConfig{
		URL: server.URL, ManagementKey: "handler-management-secret-canary",
	})
	t.Cleanup(svc.Stop)
	r := prismConfigurationHandlerRouter(NewPrismHandler(svc), false, true)
	body, err := json.Marshal(prismConfigurationHandlerOptions())
	require.NoError(t, err)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			requestBody := ""
			if method == http.MethodPut {
				requestBody = string(body)
			}
			w := prismHandlerTestRequest(r, method, "/api/v1/admin/settings/prism", requestBody)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "handler-management-secret-canary")
			var envelope struct {
				Data service.PrismConfigurationResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
			require.Equal(t, "ready", envelope.Data.Availability)
			require.Equal(t, state, *envelope.Data.Configuration)
			require.True(t, envelope.Data.Gateway.Configured)
			require.True(t, envelope.Data.Gateway.ManagementKeyConfigured)
			require.Equal(t, server.URL, envelope.Data.Gateway.BaseURL)
		})
	}
}

func TestPrismConfigurationHandlerRejectsInvalidBodyBeforeUpstream(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	svc := service.NewPrismAccountService(nil, nil, nil, service.PrismRuntimeConfig{URL: server.URL, ManagementKey: "secret-canary"})
	t.Cleanup(svc.Stop)
	r := prismConfigurationHandlerRouter(NewPrismHandler(svc), false, true)
	valid, err := json.Marshal(prismConfigurationHandlerOptions())
	require.NoError(t, err)
	for _, body := range []string{
		`{}`, `null`, `[]`, string(valid) + `{}`,
		strings.TrimSuffix(string(valid), "}") + `,"base_url":"http://injected-canary.invalid"}`,
		strings.TrimSuffix(string(valid), "}") + `,"management_key":"injected-secret-canary"}`,
		strings.Replace(string(valid), `"http_cache":false`, `"http_cache":null`, 1),
		strings.Replace(string(valid), `"memory_limit_mib":0`, `"memory_limit_mib":32`, 1),
		string(valid) + strings.Repeat(" ", service.PrismConfigurationRequestLimit),
	} {
		w := prismHandlerTestRequest(r, http.MethodPut, "/api/v1/admin/settings/prism", body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "PRISM_SETTINGS_INVALID")
		require.NotContains(t, w.Body.String(), "canary")
	}
	require.Zero(t, calls.Load())
}

func TestPrismConfigurationHandlerObserver403BeforeAnyUpstream(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	svc := service.NewPrismAccountService(nil, nil, nil, service.PrismRuntimeConfig{URL: server.URL, ManagementKey: "secret-canary"})
	t.Cleanup(svc.Stop)
	for _, routeGuard := range []bool{true, false} {
		r := prismConfigurationHandlerRouter(NewPrismHandler(svc), true, routeGuard)
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			w := prismHandlerTestRequest(r, method, "/api/v1/admin/settings/prism", `{}`)
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), server.URL)
		}
	}
	require.Zero(t, calls.Load())
}

func TestPrismConfigurationHandlerNotConfiguredAndUnsupported(t *testing.T) {
	r := prismConfigurationHandlerRouter(NewPrismHandler(nil), false, true)
	w := prismHandlerTestRequest(r, http.MethodGet, "/api/v1/admin/settings/prism", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"availability":"not_configured"`)
	require.Contains(t, w.Body.String(), `"configuration":null`)
	w = prismHandlerTestRequest(r, http.MethodDelete, "/api/v1/admin/settings/prism", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "PRISM_SETTINGS_NOT_CONFIGURED")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "private-upstream-token-canary")
	}))
	defer server.Close()
	svc := service.NewPrismAccountService(nil, nil, nil, service.PrismRuntimeConfig{URL: server.URL, ManagementKey: "secret-canary"})
	t.Cleanup(svc.Stop)
	r = prismConfigurationHandlerRouter(NewPrismHandler(svc), false, true)
	w = prismHandlerTestRequest(r, http.MethodGet, "/api/v1/admin/settings/prism", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"availability":"unsupported"`)
	require.Contains(t, w.Body.String(), `"configuration":null`)
	w = prismHandlerTestRequest(r, http.MethodDelete, "/api/v1/admin/settings/prism", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "PRISM_SETTINGS_UNSUPPORTED")
	require.NotContains(t, w.Body.String(), "canary")
}

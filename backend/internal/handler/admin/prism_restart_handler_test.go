package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type prismRestartHandlerTestService struct {
	result       *service.PrismRestartResult
	err          error
	statusCalls  int
	restartCalls int
	request      service.PrismRestartRequest
}

func (s *prismRestartHandlerTestService) GetRestartStatus(context.Context) (*service.PrismRestartResult, error) {
	s.statusCalls++
	return s.result, s.err
}

func (s *prismRestartHandlerTestService) Restart(_ context.Context, request service.PrismRestartRequest) (*service.PrismRestartResult, error) {
	s.restartCalls++
	s.request = request
	return s.result, s.err
}

func prismRestartHandlerRouter(h *PrismHandler, observer, routeGuard bool) *gin.Engine {
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
	r.GET("/api/v1/admin/settings/prism/restart", h.GetRestartStatus)
	r.POST("/api/v1/admin/settings/prism/restart", h.Restart)
	return r
}

func prismRestartHandlerRequest() service.PrismRestartRequest {
	return service.PrismRestartRequest{
		ExpectedRuntimeID:     "2f7de638-6240-4a81-91e3-639f4b853f32",
		ExpectedConfiguration: prismConfigurationHandlerOptions(),
	}
}

func prismRestartHandlerBody(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(prismRestartHandlerRequest())
	require.NoError(t, err)
	return string(body)
}

func TestPrismRestartHandlerStatusAndAcceptedRestart(t *testing.T) {
	for _, test := range []struct {
		name, method, state string
		wantStatus          int
	}{
		{"status", http.MethodGet, "ready", http.StatusOK},
		{"restart", http.MethodPost, "restarting", http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := &prismRestartHandlerTestService{result: &service.PrismRestartResult{
				Availability: "ready",
				Runtime:      &service.PrismRestartRuntime{Supported: true, RuntimeID: "2f7de638-6240-4a81-91e3-639f4b853f32", State: test.state},
			}}
			router := prismRestartHandlerRouter(&PrismHandler{restartService: svc}, false, true)
			w := prismHandlerTestRequest(router, test.method, "/api/v1/admin/settings/prism/restart", prismRestartHandlerBody(t))
			require.Equal(t, test.wantStatus, w.Code, w.Body.String())
			var envelope struct {
				Code int                        `json:"code"`
				Data service.PrismRestartResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
			require.Zero(t, envelope.Code)
			require.Equal(t, *svc.result, envelope.Data)
			require.NotContains(t, w.Body.String(), `"gateway":`)
			require.NotContains(t, w.Body.String(), `"configuration":`)
			if test.method == http.MethodGet {
				require.Equal(t, 1, svc.statusCalls)
				require.Zero(t, svc.restartCalls)
			} else {
				require.Equal(t, 1, svc.restartCalls)
				require.Zero(t, svc.statusCalls)
				require.Equal(t, prismRestartHandlerRequest(), svc.request)
			}
		})
	}
}

func TestPrismRestartHandlerConstructorWiresManagementService(t *testing.T) {
	request := prismRestartHandlerRequest()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/restart", r.URL.Path)
		require.Equal(t, "Bearer management-secret-canary", r.Header.Get("Authorization"))
		runtime := service.PrismRestartRuntime{Supported: true, RuntimeID: request.ExpectedRuntimeID, State: "ready"}
		if r.Method == http.MethodPost {
			var received service.PrismRestartRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
			require.Equal(t, request, received)
			runtime.State = "restarting"
			w.WriteHeader(http.StatusAccepted)
		}
		require.NoError(t, json.NewEncoder(w).Encode(runtime))
	}))
	defer server.Close()
	svc := service.NewPrismAccountService(nil, nil, nil, service.PrismRuntimeConfig{
		URL: server.URL, ManagementKey: "management-secret-canary",
	})
	t.Cleanup(svc.Stop)
	router := prismRestartHandlerRouter(NewPrismHandler(svc), false, true)
	for _, test := range []struct {
		method string
		status int
	}{
		{http.MethodGet, http.StatusOK},
		{http.MethodPost, http.StatusAccepted},
	} {
		w := prismHandlerTestRequest(router, test.method, "/api/v1/admin/settings/prism/restart", prismRestartHandlerBody(t))
		require.Equal(t, test.status, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `"availability":"ready"`)
		require.Contains(t, w.Body.String(), request.ExpectedRuntimeID)
		require.NotContains(t, w.Body.String(), "canary")
	}
}

func TestPrismRestartHandlerStatusPreservesUnavailableEnvelope(t *testing.T) {
	for _, availability := range []string{"not_configured", "unsupported", "unavailable"} {
		t.Run(availability, func(t *testing.T) {
			svc := &prismRestartHandlerTestService{result: &service.PrismRestartResult{Availability: availability}}
			w := prismHandlerTestRequest(prismRestartHandlerRouter(&PrismHandler{restartService: svc}, false, true),
				http.MethodGet, "/api/v1/admin/settings/prism/restart", "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `"availability":"`+availability+`"`)
			require.Contains(t, w.Body.String(), `"runtime":null`)
			require.Equal(t, 1, svc.statusCalls)
			require.Zero(t, svc.restartCalls)
		})
	}
}

type prismRestartUnreadBody struct {
	reads int
}

func (b *prismRestartUnreadBody) Read([]byte) (int, error) {
	b.reads++
	return 0, errors.New("body-read-secret-canary")
}

func (*prismRestartUnreadBody) Close() error { return nil }

func TestPrismRestartHandlerObserverGuardPrecedesBodyAndService(t *testing.T) {
	for _, routeGuard := range []bool{true, false} {
		for _, configured := range []bool{true, false} {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				t.Run(fmt.Sprintf("route_guard_%v/configured_%v/%s", routeGuard, configured, method), func(t *testing.T) {
					svc := &prismRestartHandlerTestService{}
					h := NewPrismHandler(nil)
					if configured {
						h.restartService = svc
					}
					router := prismRestartHandlerRouter(h, true, routeGuard)
					body := &prismRestartUnreadBody{}
					req := httptest.NewRequest(method, "/api/v1/admin/settings/prism/restart", nil)
					req.Body = body
					req.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					router.ServeHTTP(w, req)
					require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
					require.Contains(t, w.Body.String(), "OBSERVER_SCOPE_FORBIDDEN")
					require.NotContains(t, w.Body.String(), "canary")
					require.Zero(t, body.reads, "observer scope must be checked before parsing the body")
					require.Zero(t, svc.statusCalls)
					require.Zero(t, svc.restartCalls)
				})
			}
		}
	}
}

func TestPrismRestartHandlerNotConfigured(t *testing.T) {
	for _, h := range []*PrismHandler{nil, NewPrismHandler(nil)} {
		router := prismRestartHandlerRouter(h, false, true)
		w := prismHandlerTestRequest(router, http.MethodGet, "/api/v1/admin/settings/prism/restart", "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `"availability":"not_configured"`)
		require.Contains(t, w.Body.String(), `"runtime":null`)
		w = prismHandlerTestRequest(router, http.MethodPost, "/api/v1/admin/settings/prism/restart", prismRestartHandlerBody(t))
		require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "PRISM_RESTART_NOT_CONFIGURED")
	}
}

func TestPrismRestartHandlerRejectsStrictBodyBeforeService(t *testing.T) {
	valid := prismRestartHandlerBody(t)
	for _, test := range []struct{ name, body string }{
		{"empty", ""},
		{"malformed", `{body-secret-canary`},
		{"empty_object", `{}`},
		{"null", `null`},
		{"array", `[]`},
		{"trailing_document", valid + `{}`},
		{"missing_runtime", `{"expected_configuration":` + mustPrismRestartConfigurationJSON(t) + `}`},
		{"missing_configuration", `{"expected_runtime_id":"2f7de638-6240-4a81-91e3-639f4b853f32"}`},
		{"empty_runtime", strings.Replace(valid, `"2f7de638-6240-4a81-91e3-639f4b853f32"`, `""`, 1)},
		{"null_runtime", strings.Replace(valid, `"2f7de638-6240-4a81-91e3-639f4b853f32"`, `null`, 1)},
		{"unknown_field", strings.TrimSuffix(valid, "}") + `,"management_key":"request-secret-canary"}`},
		{"case_variant", strings.Replace(valid, `"expected_runtime_id"`, `"Expected_runtime_id"`, 1)},
		{"duplicate_runtime", strings.TrimSuffix(valid, "}") + `,"expected_runtime_id":"2f7de638-6240-4a81-91e3-639f4b853f32"}`},
		{"duplicate_configuration", strings.TrimSuffix(valid, "}") + `,"expected_configuration":` + mustPrismRestartConfigurationJSON(t) + `}`},
		{"null_option", strings.Replace(valid, `"http_cache":false`, `"http_cache":null`, 1)},
		{"unknown_configuration_option", strings.Replace(valid, `"http_cache":false`, `"http_cache":false,"base_url":"http://request-secret-canary.invalid"`, 1)},
		{"duplicate_configuration_option", strings.Replace(valid, `"http_cache":false`, `"http_cache":false,"http_cache":true`, 1)},
		{"invalid_memory", strings.Replace(valid, `"memory_limit_mib":0`, `"memory_limit_mib":32`, 1)},
		{"oversized", valid + strings.Repeat(" ", service.PrismRestartRequestLimit)},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := &prismRestartHandlerTestService{}
			w := prismHandlerTestRequest(prismRestartHandlerRouter(&PrismHandler{restartService: svc}, false, true), http.MethodPost,
				"/api/v1/admin/settings/prism/restart", test.body)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "PRISM_RESTART_INVALID")
			require.NotContains(t, w.Body.String(), "canary")
			require.Zero(t, svc.statusCalls)
			require.Zero(t, svc.restartCalls)
		})
	}
}

func mustPrismRestartConfigurationJSON(t *testing.T) string {
	t.Helper()
	configuration, err := json.Marshal(prismConfigurationHandlerOptions())
	require.NoError(t, err)
	return string(configuration)
}

func TestPrismRestartHandlerRejectsUnreadableBody(t *testing.T) {
	svc := &prismRestartHandlerTestService{}
	router := prismRestartHandlerRouter(&PrismHandler{restartService: svc}, false, true)
	body := &prismRestartUnreadBody{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/settings/prism/restart", nil)
	req.Body = body
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "PRISM_RESTART_INVALID")
	require.NotContains(t, w.Body.String(), "canary")
	require.Positive(t, body.reads)
	require.Zero(t, svc.restartCalls)
}

func TestPrismRestartHandlerServiceErrorsAreSafe(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		err          error
		wantStatus   int
	}{
		{"invalid", "PRISM_RESTART_INVALID", infraerrors.New(http.StatusBadRequest, "PRISM_RESTART_INVALID", "upstream-secret-canary"), http.StatusBadRequest},
		{"conflict", "PRISM_RESTART_CONFLICT", infraerrors.New(http.StatusConflict, "PRISM_RESTART_CONFLICT", "upstream-secret-canary"), http.StatusConflict},
		{"unsupported", "PRISM_RESTART_UNSUPPORTED", infraerrors.New(http.StatusServiceUnavailable, "PRISM_RESTART_UNSUPPORTED", "upstream-secret-canary"), http.StatusServiceUnavailable},
		{"not_configured", "PRISM_RESTART_NOT_CONFIGURED", infraerrors.New(http.StatusServiceUnavailable, "PRISM_RESTART_NOT_CONFIGURED", "upstream-secret-canary"), http.StatusServiceUnavailable},
		{"unavailable", "PRISM_RESTART_UNAVAILABLE", infraerrors.New(http.StatusServiceUnavailable, "PRISM_RESTART_UNAVAILABLE", "upstream-secret-canary"), http.StatusServiceUnavailable},
		{"observer", "OBSERVER_SCOPE_FORBIDDEN", fmt.Errorf("upstream-secret-canary: %w", service.ErrObserverScope), http.StatusForbidden},
		{"unknown", "PRISM_RESTART_UNAVAILABLE", errors.New("upstream-secret-canary"), http.StatusServiceUnavailable},
		{"untrusted_application", "PRISM_RESTART_UNAVAILABLE", infraerrors.New(http.StatusBadGateway, "UPSTREAM_SECRET_CANARY", "upstream-secret-canary"), http.StatusServiceUnavailable},
		{"mismatched_public_reason", "PRISM_RESTART_UNAVAILABLE", infraerrors.New(http.StatusBadGateway, "PRISM_RESTART_CONFLICT", "upstream-secret-canary"), http.StatusServiceUnavailable},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(test.name+"/"+method, func(t *testing.T) {
				err := test.err
				if appErr, ok := err.(*infraerrors.ApplicationError); ok {
					err = appErr.WithMetadata(map[string]string{"upstream": "metadata-secret-canary"}).WithCause(errors.New("cause-secret-canary"))
				}
				svc := &prismRestartHandlerTestService{err: err}
				w := prismHandlerTestRequest(prismRestartHandlerRouter(&PrismHandler{restartService: svc}, false, true), method,
					"/api/v1/admin/settings/prism/restart", prismRestartHandlerBody(t))
				require.Equal(t, test.wantStatus, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), test.reason)
				require.NotContains(t, w.Body.String(), "canary")
				require.NotContains(t, w.Body.String(), "CANARY")
				require.NotContains(t, w.Body.String(), `"metadata":`)
				require.NotContains(t, w.Body.String(), `"runtime":`)
			})
		}
	}
}

var _ io.ReadCloser = (*prismRestartUnreadBody)(nil)

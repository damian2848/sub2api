package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type prismHandlerTestService struct {
	account *service.Account
	status  *service.PrismStatus
	err     error
	calls   int
	id      int64
	name    string
	groups  []int64
}

func (s *prismHandlerTestService) Create(_ context.Context, id int64, name string, groups []int64) (*service.Account, *service.PrismStatus, error) {
	s.calls++
	s.id, s.name, s.groups = id, name, groups
	return s.account, s.status, s.err
}

func (s *prismHandlerTestService) Status(_ context.Context, id int64) (*service.PrismStatus, error) {
	s.calls++
	s.id = id
	return s.status, s.err
}

func (s *prismHandlerTestService) Reconnect(_ context.Context, id int64) (*service.PrismStatus, error) {
	s.calls++
	s.id = id
	return s.status, s.err
}

func prismHandlerTestRouter(h *PrismHandler, observerGroups []int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if observerGroups != nil {
		r.Use(func(c *gin.Context) {
			c.Request = c.Request.WithContext(service.WithObserverScope(c.Request.Context(), observerGroups))
		})
	}
	r.POST("/accounts/:id/prism", h.Create)
	r.GET("/accounts/:id/prism/status", h.Status)
	r.POST("/accounts/:id/prism/reconnect", h.Reconnect)
	return r
}

func prismHandlerTestRequest(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPrismHandlerCreateReturnsAcceptedWithoutCredentials(t *testing.T) {
	svc := &prismHandlerTestService{
		account: &service.Account{ID: 101, Name: "OAuth source (Prism)", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, GroupIDs: []int64{10, 20},
			Credentials: map[string]any{"api_key": "private-adapter-api-key", "access_token": "private-access-token",
				"refresh_token": "private-refresh-token", "base_url": "http://127.0.0.1:8319/accounts/32/v1"},
			Extra: map[string]any{"provider_preset": service.PrismProviderPreset, service.PrismSourceAccountKey: int64(32)}},
		status: &service.PrismStatus{Phase: "provisioning", Models: []string{}, AccountID: 101,
			SourceAccountID: 32, Enabled: true},
	}
	router := prismHandlerTestRouter(&PrismHandler{prismService: svc}, []int64{10})
	w := prismHandlerTestRequest(router, http.MethodPost, "/accounts/32/prism", `{"name":"custom Prism","group_ids":[10]}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, int64(32), svc.id)
	require.Equal(t, "custom Prism", svc.name)
	require.Equal(t, []int64{10}, svc.groups)
	for _, secret := range []string{"private-adapter-api-key", "private-access-token", "private-refresh-token"} {
		require.NotContains(t, w.Body.String(), secret)
	}
	var envelope struct {
		Data struct {
			Account struct {
				ID          int64          `json:"id"`
				GroupIDs    []int64        `json:"group_ids"`
				Schedulable bool           `json:"schedulable"`
				Credentials map[string]any `json:"credentials"`
			} `json:"account"`
			Status service.PrismStatus `json:"status"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, int64(101), envelope.Data.Account.ID)
	require.Equal(t, []int64{10}, envelope.Data.Account.GroupIDs)
	require.Equal(t, []int64{10, 20}, svc.account.GroupIDs, "observer response filtering must not mutate the stored account")
	require.False(t, envelope.Data.Account.Schedulable)
	require.NotContains(t, envelope.Data.Account.Credentials, "api_key")
	require.Equal(t, "provisioning", envelope.Data.Status.Phase)
	require.False(t, envelope.Data.Status.Ready)
}

func TestPrismHandlerValidatesInputBeforeServiceCalls(t *testing.T) {
	for _, test := range []struct{ name, method, path, body string }{
		{"invalid_source", http.MethodPost, "/accounts/nope/prism", `{}`},
		{"zero_source", http.MethodPost, "/accounts/0/prism", `{}`},
		{"negative_source", http.MethodPost, "/accounts/-32/prism", `{}`},
		{"bad_json", http.MethodPost, "/accounts/32/prism", `{bad`},
		{"long_name", http.MethodPost, "/accounts/32/prism", `{"name":"` + strings.Repeat("x", 101) + `"}`},
		{"invalid_group", http.MethodPost, "/accounts/32/prism", `{"group_ids":[-10]}`},
		{"invalid_status_id", http.MethodGet, "/accounts/nope/prism/status", ""},
		{"invalid_reconnect_id", http.MethodPost, "/accounts/0/prism/reconnect", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := &prismHandlerTestService{}
			w := prismHandlerTestRequest(prismHandlerTestRouter(&PrismHandler{prismService: svc}, nil), test.method, test.path, test.body)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Zero(t, svc.calls)
		})
	}
}

func TestPrismHandlerStatusAndReconnectUseManagedAccountID(t *testing.T) {
	for _, test := range []struct {
		name, method, suffix string
		wantStatus           int
	}{
		{"status", http.MethodGet, "status", http.StatusOK},
		{"reconnect", http.MethodPost, "reconnect", http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := &prismHandlerTestService{status: &service.PrismStatus{Phase: "provisioning", Ready: false,
				AccountID: 101, SourceAccountID: 32, Enabled: true, Models: []string{}}}
			w := prismHandlerTestRequest(prismHandlerTestRouter(&PrismHandler{prismService: svc}, nil),
				test.method, "/accounts/101/prism/"+test.suffix, "")
			require.Equal(t, test.wantStatus, w.Code, w.Body.String())
			require.Equal(t, int64(101), svc.id)
			require.Equal(t, 1, svc.calls)
			var envelope struct {
				Data service.PrismStatus `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
			require.Equal(t, int64(32), envelope.Data.SourceAccountID)
			require.False(t, envelope.Data.Ready)
		})
	}
}

func TestPrismHandlerUnconfiguredServiceAndScopeErrors(t *testing.T) {
	for _, test := range []struct{ method, path string }{
		{http.MethodPost, "/accounts/32/prism"},
		{http.MethodGet, "/accounts/101/prism/status"},
		{http.MethodPost, "/accounts/101/prism/reconnect"},
	} {
		w := prismHandlerTestRequest(prismHandlerTestRouter(NewPrismHandler(nil), nil), test.method, test.path, "")
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		require.Contains(t, w.Body.String(), "PRISM_NOT_CONFIGURED")
	}
	svc := &prismHandlerTestService{err: service.ErrObserverScope}
	w := prismHandlerTestRequest(prismHandlerTestRouter(&PrismHandler{prismService: svc}, nil), http.MethodPost, "/accounts/32/prism", `{}`)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.NotContains(t, w.Body.String(), `"account":`)
	svc.err = infraerrors.BadRequest("PRISM_SOURCE_INVALID", "Prism requires an existing OpenAI OAuth account")
	w = prismHandlerTestRequest(prismHandlerTestRouter(&PrismHandler{prismService: svc}, nil), http.MethodPost, "/accounts/32/prism", `{}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "PRISM_SOURCE_INVALID")
}

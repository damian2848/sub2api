//go:build unit

package admin

import (
	"context"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"strings"
	"testing"
)

type opsHandlerSettings struct{ service.SettingRepository }

func (opsHandlerSettings) GetValue(context.Context, string) (string, error) { return "", nil }
func TestAccountOpsConfigWithoutSMTPService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := service.NewAccountOpsService(opsHandlerSettings{}, nil, nil)
	h := NewAccountOpsHandler(svc, nil)
	r := gin.New()
	r.GET("/config", h.GetConfig)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/config", nil))
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"smtp_configured":false`)
}

type opsHandlerWritableSettings struct {
	service.SettingRepository
	raw      string
	writes   int
	writeErr error
}

func (s *opsHandlerWritableSettings) GetValue(context.Context, string) (string, error) {
	return s.raw, nil
}
func (s *opsHandlerWritableSettings) Set(_ context.Context, _ string, value string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.raw = value
	s.writes++
	return nil
}

type opsHandlerAccounts struct {
	service.AccountRepository
	account *service.Account
	err     error
}

func (s opsHandlerAccounts) GetByID(context.Context, int64) (*service.Account, error) {
	return s.account, s.err
}

type opsHandlerQueue struct{ service.AccountOpsRepository }

func (opsHandlerQueue) SuppressDisabled(context.Context, service.AccountOpsConfig) error { return nil }

type opsHandlerEncryptor struct{ err error }

func (s opsHandlerEncryptor) Encrypt(string) (string, error) { return "synthetic-cipher", s.err }
func (s opsHandlerEncryptor) Decrypt(string) (string, error) { return "", s.err }
func TestAccountOpsInvalidConfigurationReturnsSafe400WithoutWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, fields string
		accountType  string
	}{
		{"amount rule on OAuth", `,"balance_thresholds":[{"account_id":41,"enabled":true,"threshold":5,"unit":"USD"}]`, service.AccountTypeOAuth},
		{"quota rule on API key", `,"quota_thresholds":[{"account_id":41,"enabled":true,"threshold_percent":80,"window":"any"}]`, service.AccountTypeAPIKey},
		{"invalid recipient", `,"recipient":"canary-secret@example.test,another@example.test"`, service.AccountTypeAPIKey},
		{"invalid official URL", `,"webhooks":[{"id":"robot","provider":"wecom","url":"https://bad.example/canary-secret?key=private-canary"}]`, service.AccountTypeAPIKey},
		{"negative threshold", `,"balance_thresholds":[{"account_id":41,"enabled":true,"threshold":-1,"unit":"USD"}]`, service.AccountTypeAPIKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := &opsHandlerWritableSettings{raw: `{"cooldown_minutes":60}`}
			before := settings.raw
			svc := service.NewAccountOpsService(settings, opsHandlerQueue{}, nil)
			svc.SetNotificationDependencies(opsHandlerAccounts{account: &service.Account{ID: 41, Type: tc.accountType}}, opsHandlerEncryptor{}, true, "Asia/Shanghai")
			h := NewAccountOpsHandler(svc, nil)
			router := gin.New()
			router.PUT("/config", h.SaveConfig)
			request := httptest.NewRequest("PUT", "/config", strings.NewReader(`{"enabled":false,"cooldown_minutes":60`+tc.fields+`}`))
			request.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, request)
			require.Equal(t, 400, rec.Code)
			require.Equal(t, before, settings.raw)
			require.Zero(t, settings.writes)
			require.NotContains(t, rec.Body.String(), "canary-secret")
			require.NotContains(t, rec.Body.String(), "private-canary")
			require.NotContains(t, rec.Body.String(), "bad.example")
		})
	}
}
func TestAccountOpsInfrastructureFailuresReturnSafe503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"storage", "account lookup", "encryption", "stored config"} {
		t.Run(kind, func(t *testing.T) {
			settings := &opsHandlerWritableSettings{raw: `{"cooldown_minutes":60}`}
			lookup := opsHandlerAccounts{account: &service.Account{ID: 41, Type: service.AccountTypeAPIKey}}
			encryptor := opsHandlerEncryptor{}
			body := `{"enabled":false,"cooldown_minutes":60}`
			canary := errors.New("database-or-credential-private-canary")
			switch kind {
			case "stored config":
				settings.raw = `{"enabled":true,"cooldown_minutes":60}`
			case "storage":
				settings.writeErr = canary
			case "account lookup":
				lookup.err = canary
				body = `{"enabled":false,"cooldown_minutes":60,"balance_thresholds":[{"account_id":41,"enabled":true,"threshold":5,"unit":"USD"}]}`
			case "encryption":
				encryptor.err = canary
				body = `{"enabled":false,"cooldown_minutes":60,"webhooks":[{"id":"robot","provider":"wecom","url":"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=private-canary"}]}`
			}
			svc := service.NewAccountOpsService(settings, opsHandlerQueue{}, nil)
			svc.SetNotificationDependencies(lookup, encryptor, true, "Asia/Shanghai")
			router := gin.New()
			router.PUT("/config", NewAccountOpsHandler(svc, nil).SaveConfig)
			request := httptest.NewRequest("PUT", "/config", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, request)
			require.Equal(t, 503, rec.Code)
			require.NotContains(t, rec.Body.String(), "private-canary")
			require.Zero(t, settings.writes)
		})
	}
}

func TestAccountOpsScopedRequestsIgnoreOtherScopesAndSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := &opsHandlerWritableSettings{raw: `{"cooldown_minutes":60,"balance_thresholds":[{"account_id":41,"enabled":true,"threshold":5,"unit":"USD","notify_alert":false,"notify_recovery":true}],"encrypted_webhooks":[{"id":"saved","provider":"dingtalk","enabled":true,"url_cipher":"private-url-cipher","secret_cipher":"private-secret-cipher","revision":"unchanged"}]}`}
	svc := service.NewAccountOpsService(settings, opsHandlerQueue{}, nil)
	svc.SetNotificationDependencies(opsHandlerAccounts{account: &service.Account{ID: 41, Type: service.AccountTypeAPIKey}}, opsHandlerEncryptor{}, true, "UTC")
	router := gin.New()
	h := NewAccountOpsHandler(svc, nil)
	router.PUT("/notification-settings", h.SaveNotificationSettings)
	router.PUT("/rules/:id", h.SaveRule)
	router.DELETE("/rules/:id", h.DeleteRule)
	send := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec
	}
	rec := send("PUT", "/notification-settings", `{"enabled":false,"recipient":"new@example.test","balance_low":true,"weekly_quota":true,"cooldown_minutes":90,"webhooks":[],"balance_thresholds":[],"secret":"must-not-save"}`)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"account_id":41`)
	require.Contains(t, rec.Body.String(), `"id":"saved"`)
	require.NotContains(t, rec.Body.String(), "private-")
	require.NotContains(t, settings.raw, "must-not-save")
	require.Contains(t, settings.raw, "private-secret-cipher")
	rec = send("PUT", "/rules/41", `{"metric":"balance","enabled":true,"threshold":9,"notify_recovery":false,"recipient":"overwrite@example.test","webhooks":[],"url":"must-not-save","secret":"must-not-save"}`)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"recipient":"new@example.test"`)
	require.Contains(t, rec.Body.String(), `"notify_alert":false`)
	require.Contains(t, rec.Body.String(), `"notify_recovery":false`)
	require.Contains(t, settings.raw, "private-secret-cipher")
	require.NotContains(t, settings.raw, "must-not-save")
	rec = send("DELETE", "/rules/41?metric=balance", "")
	require.Equal(t, 200, rec.Code)
	require.NotContains(t, rec.Body.String(), `"account_id":41`)
	require.Contains(t, rec.Body.String(), `"id":"saved"`)
}
func TestAccountOpsScopedValidationAndStorageFailuresStaySafe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name    string
		body    string
		storage bool
		want    int
	}{
		{"static threshold validation", `{"metric":"balance","enabled":true,"threshold":-1,"unit":"USD"}`, false, 400},
		{"database failure", `{"metric":"balance","enabled":true,"threshold":5,"unit":"USD"}`, true, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := &opsHandlerWritableSettings{raw: `{"cooldown_minutes":60}`}
			if tc.storage {
				settings.writeErr = errors.New("private-database-canary")
			}
			svc := service.NewAccountOpsService(settings, opsHandlerQueue{}, nil)
			svc.SetNotificationDependencies(opsHandlerAccounts{account: &service.Account{ID: 41, Type: service.AccountTypeAPIKey}}, nil, false, "UTC")
			router := gin.New()
			router.PUT("/rules/:id", NewAccountOpsHandler(svc, nil).SaveRule)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("PUT", "/rules/41", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code)
			require.Zero(t, settings.writes)
			require.NotContains(t, rec.Body.String(), "private-database-canary")
		})
	}
}

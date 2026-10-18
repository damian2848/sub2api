package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type cannedSettingRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r *cannedSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if v, ok := r.values[key]; ok {
		return v, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *cannedSettingRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

func TestGeminiWebCannedRepliesEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &AccountHandler{}
	h.SetCodexTicketSettings(service.NewSettingService(&cannedSettingRepo{values: map[string]string{}}, &config.Config{}))
	router := gin.New()
	router.GET("/canned", h.GetGeminiWebCannedReplies)
	router.PUT("/canned", h.UpdateGeminiWebCannedReplies)

	call := func(method, body string) (int, service.GeminiWebCannedReplySettings) {
		req := httptest.NewRequest(method, "/canned", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var envelope struct {
			Data service.GeminiWebCannedReplySettings `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
		return rec.Code, envelope.Data
	}

	code, got := call(http.MethodGet, "")
	require.Equal(t, http.StatusOK, code)
	require.ElementsMatch(t, geminiweb.BuiltInCannedReplies(), got.BuiltIn)
	require.Contains(t, got.BuiltIn, "Sorry, something went wrong. Please try your request again.")
	require.Empty(t, got.Custom)

	code, got = call(http.MethodPut, `{"custom":["  Gemini is busy.  ",""]}`)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"Gemini is busy."}, got.Custom)

	code, _ = call(http.MethodPut, `{"custom":["`+strings.Repeat("x", 301)+`"]}`)
	require.Equal(t, http.StatusBadRequest, code)
	code, got = call(http.MethodGet, "")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"Gemini is busy."}, got.Custom, "a rejected update keeps the saved list")
}

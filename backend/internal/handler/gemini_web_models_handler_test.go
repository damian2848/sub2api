//go:build unit

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/gemini"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiWebGetModelHandlerPreservesEffectiveModelPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name           string
		model          string
		mapping        map[string]any
		status         int
		staticFallback bool
	}{
		{name: "default canonical", model: "gemini-4-argon", status: http.StatusOK},
		{name: "default alias", model: "gemini-4-argon-low", status: http.StatusOK},
		{name: "default scope rejects static model", model: "gemini-3.1-pro-preview", status: http.StatusNotFound, staticFallback: true},
		{name: "custom alias", model: "custom-pro", mapping: map[string]any{"custom-pro": "gemini-3.1-pro"}, status: http.StatusOK},
		{name: "custom scope rejects default", model: "gemini-4-argon", mapping: map[string]any{"custom-pro": "gemini-3.1-pro"}, status: http.StatusNotFound, staticFallback: true},
		{name: "custom scope rejects target name", model: "gemini-3.1-pro", mapping: map[string]any{"custom-pro": "gemini-3.1-pro"}, status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(52)
			account := service.Account{ID: 288, Platform: service.PlatformGemini, Type: service.AccountTypeGeminiWeb,
				Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{}}
			if tc.mapping != nil {
				account.Credentials["model_mapping"] = tc.mapping
			}
			repo := &geminiAllowlistAccountRepoStub{gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{groupID: {account}}}}
			h := &GatewayHandler{geminiCompatService: service.NewGeminiMessagesCompatService(repo, nil, nil, nil, nil, nil, nil, nil, nil)}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models/"+tc.model, nil)
			c.Params = gin.Params{{Key: "model", Value: tc.model}}
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
				ID: 1, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformGemini},
			})
			h.GeminiV1BetaGetModel(c)
			require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
			if tc.status == http.StatusOK {
				var model gemini.Model
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &model))
				require.Equal(t, "models/"+tc.model, model.Name)
			} else {
				if tc.staticFallback {
					require.True(t, gemini.HasFallbackModel(tc.model), "regression must exercise a model the generic fallback would revive")
				}
				require.Contains(t, recorder.Body.String(), "not found")
			}
		})
	}
}

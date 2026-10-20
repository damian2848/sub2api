//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayAntigravityThinkingCapacity_ActualRoutesUseForwardedVariant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "gemini-3.8-flash"
	messages := func(thinking string) string {
		return fmt.Sprintf(`{"model":%q,"stream":true,"max_tokens":16384,"messages":[{"role":"user","content":"hello"}]%s}`, model, thinking)
	}
	chat := func(reasoning string) string {
		return fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hello"}]%s}`, model, reasoning)
	}
	responses := func(reasoning string) string {
		return fmt.Sprintf(`{"model":%q,"stream":true,"input":"hello"%s}`, model, reasoning)
	}
	native := func(thinking string) string {
		return `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]` + thinking + `}`
	}
	for _, tc := range []struct {
		name, protocol, body string
		level                string
		geminiGroup          bool
	}{
		{"native default", "gemini", native(""), "high", false},
		{"native explicit low", "gemini", native(`,"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}`), "low", false},
		{"native budget medium", "gemini", native(`,"generationConfig":{"thinkingConfig":{"thinkingBudget":4096}}`), "medium", false},
		{"native budget zero", "gemini", native(`,"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}`), "low", false},
		{"messages default", "messages", messages(""), "high", false},
		{"messages disabled", "messages", messages(`,"thinking":{"type":"disabled"}`), "low", false},
		{"messages low budget", "messages", messages(`,"thinking":{"type":"enabled","budget_tokens":1024}`), "low", false},
		{"messages medium budget", "messages", messages(`,"thinking":{"type":"enabled","budget_tokens":4096}`), "medium", false},
		{"messages high budget", "messages", messages(`,"thinking":{"type":"enabled","budget_tokens":10240}`), "high", false},
		{"messages Gemini branch", "messages", messages(`,"thinking":{"type":"enabled","budget_tokens":4096}`), "medium", true},
		{"chat default", "chat_completions", chat(""), "high", false},
		// The compatibility converter omits Claude thinking for effort=low,
		// so the existing forwarder uses the default high variant in this case.
		{"chat low effort keeps converter semantics", "chat_completions", chat(`,"reasoning_effort":"low"`), "high", false},
		{"chat medium effort", "chat_completions", chat(`,"reasoning_effort":"medium"`), "medium", false},
		{"chat nested medium effort", "chat_completions", chat(`,"reasoning":{"effort":"medium"}`), "medium", false},
		{"responses default", "responses", responses(""), "high", false},
		{"responses low effort keeps converter semantics", "responses", responses(`,"reasoning":{"effort":"low"}`), "high", false},
		{"responses medium effort", "responses", responses(`,"reasoning":{"effort":"medium"}`), "medium", false},
		{"count tokens medium budget", "count_tokens", messages(`,"thinking":{"type":"enabled","budget_tokens":4096}`), "medium", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(52)
			platform := service.PlatformAntigravity
			if tc.geminiGroup {
				platform = service.PlatformGemini
			}
			group := &service.Group{ID: groupID, Hydrated: true, Platform: platform, Status: service.StatusActive}
			now := time.Now().UTC().Truncate(time.Second)
			delays := map[string]int{"low": 90, "medium": 180, "high": 270}
			mapping := map[string]any{}
			limits := map[string]any{}
			for level, delay := range delays {
				variant := model + "-" + level
				mapping[variant] = variant
				limits[variant] = map[string]any{"rate_limit_reset_at": now.Add(time.Duration(delay) * time.Second).Format(time.RFC3339)}
			}
			account := &service.Account{
				ID: 288, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, Schedulable: true, Concurrency: 4,
				AccountGroups: []service.AccountGroup{{AccountID: 288, GroupID: groupID}},
				Credentials:   map[string]any{"model_mapping": mapping},
				Extra:         map[string]any{"mixed_scheduling": true, "model_rate_limits": limits},
			}
			accounts := []*service.Account{account}
			h, cleanup := newTestGatewayHandler(t, group, accounts)
			t.Cleanup(cleanup)
			repo := &gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{groupID: {*account}}}
			snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
			h.gatewayService = service.NewGatewayService(
				repo, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
				snapshot, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			)
			path := "/v1/messages"
			call := h.Messages
			switch tc.protocol {
			case "gemini":
				path = "/antigravity/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
				call = h.GeminiV1BetaModels
			case "chat_completions":
				path = "/v1/chat/completions"
				call = h.ChatCompletions
			case "responses":
				path = "/v1/responses"
				call = h.Responses
			case "count_tokens":
				path = "/v1/messages/count_tokens"
				call = h.CountTokens
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx := context.WithValue(context.Background(), ctxkey.Group, group)
			c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(tc.body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.protocol == "gemini" {
				c.Params = gin.Params{{Key: "modelAction", Value: "/" + model + ":streamGenerateContent"}}
				c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformAntigravity)
			}
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
				ID: 1, UserID: 2, GroupID: &groupID, Group: group, Status: service.StatusActive,
				User: &service.User{ID: 2, Concurrency: 10, Balance: 100},
			})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 10})

			call(c)

			require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
			retryAfter, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
			require.NoError(t, err)
			require.InDelta(t, delays[tc.level], retryAfter, 2, "scheduler and diagnosis must use the exact variant selected by the forward converter")
			require.Contains(t, recorder.Body.String(), "Earliest retry time")
			_, selected := c.Get(opsAccountIDKey)
			require.False(t, selected, "a cooling variant must be rejected before an upstream account is admitted")
		})
	}
}

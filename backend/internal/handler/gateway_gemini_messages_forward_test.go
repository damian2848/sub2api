//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGatewayMessages_GeminiGroupAntigravityForwardsMessagesProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, accountType, model, wantModel string
		budget                              int
		stream                              bool
	}{
		{"OAuth low JSON", service.AccountTypeOAuth, "gemini-3.8-flash", "gemini-3.8-flash-low", 1024, false},
		{"OAuth medium JSON", service.AccountTypeOAuth, "gemini-3.8-flash", "gemini-3.8-flash-medium", 4096, false},
		{"OAuth low SSE", service.AccountTypeOAuth, "gemini-3.8-flash", "gemini-3.8-flash-low", 1024, true},
		{"OAuth medium SSE", service.AccountTypeOAuth, "gemini-3.8-flash", "gemini-3.8-flash-medium", 4096, true},
		{"API key retains Gemini compatibility transport", service.AccountTypeAPIKey, "gemini-2.5-flash", "gemini-2.5-flash", 1024, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(52)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformGemini, Status: service.StatusActive}
			account := &service.Account{
				ID: 288, Platform: service.PlatformAntigravity, Type: tc.accountType,
				Status: service.StatusActive, Schedulable: true, Concurrency: 4,
				AccountGroups: []service.AccountGroup{{AccountID: 288, GroupID: groupID}},
				Credentials: map[string]any{
					"access_token": "test-token", "project_id": "test-project", "api_key": "test-key",
					"model_mapping": map[string]any{
						"gemini-3.8-flash-low": "gemini-3.8-flash-low", "gemini-3.8-flash-medium": "gemini-3.8-flash-medium",
						"gemini-3.8-flash-high": "gemini-3.8-flash-high", "gemini-2.5-flash": "gemini-2.5-flash",
					},
				},
				Extra: map[string]any{
					"mixed_scheduling": true,
					"model_rate_limits": map[string]any{
						"gemini-3.8-flash-high": map[string]any{"rate_limit_reset_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)},
					},
				},
			}
			h, cleanup := newTestGatewayHandler(t, group, []*service.Account{account})
			t.Cleanup(cleanup)
			var calls int
			upstream := &geminiWaitUpstream{do: func(req *http.Request, accountID int64) (*http.Response, error) {
				calls++
				require.Equal(t, account.ID, accountID)
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.False(t, gjson.GetBytes(body, "messages").Exists(), "Claude messages must be converted before reaching Google")
				if tc.accountType == service.AccountTypeOAuth {
					require.Equal(t, tc.wantModel, gjson.GetBytes(body, "model").String())
					require.True(t, gjson.GetBytes(body, "request.contents").IsArray())
					require.Contains(t, gjson.GetBytes(body, "request.contents").Raw, "hello")
					require.Contains(t, req.URL.Path, "streamGenerateContent")
					return &http.Response{StatusCode: http.StatusOK,
						Header: http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:   io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"reply from Google\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{}}}\n\n")),
					}, nil
				}
				require.Equal(t, "test-key", req.Header.Get("x-goog-api-key"))
				require.Empty(t, req.Header.Get("Authorization"), "API-key accounts must not enter the OAuth token path")
				require.Contains(t, req.URL.Path, tc.wantModel+":generateContent")
				require.True(t, gjson.GetBytes(body, "contents").IsArray())
				return &http.Response{StatusCode: http.StatusOK,
					Header: http.Header{"Content-Type": []string{"application/json"}},
					Body:   io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"text":"reply from Google"}]},"finishReason":"STOP"}],"usageMetadata":{}}`)),
				}, nil
			}}
			if tc.accountType == service.AccountTypeOAuth {
				h.antigravityGatewayService = service.NewAntigravityGatewayService(nil, nil, nil,
					service.NewAntigravityTokenProvider(nil, nil, nil), nil, upstream,
					service.NewSettingService(&contentModerationHandlerSettingRepo{}, &config.Config{}), nil)
			}
			h.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, upstream, nil, &config.Config{})
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			body := fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":16384,"thinking":{"type":"enabled","budget_tokens":%d},"messages":[{"role":"user","content":"hello"}]}`, tc.model, tc.stream, tc.budget)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body)).
				WithContext(context.WithValue(context.Background(), ctxkey.Group, group))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
				ID: 1, UserID: 2, GroupID: &groupID, Group: group, Status: service.StatusActive,
				User: &service.User{ID: 2, Concurrency: 10, Balance: 100},
			})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 10})

			h.Messages(c)

			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Equal(t, 1, calls, "a high-only cooldown must not block or retry the selected low/medium variant")
			response := recorder.Body.String()
			require.Contains(t, response, "reply from Google")
			require.NotContains(t, response, `"candidates"`, "the client requested Anthropic Messages, not Gemini native output")
			if tc.stream {
				require.Contains(t, response, "event: message_start")
				require.Contains(t, response, "event: content_block_delta")
				require.Contains(t, response, "event: message_stop")
			} else {
				require.Equal(t, "message", gjson.Get(response, "type").String())
				require.Equal(t, "assistant", gjson.Get(response, "role").String())
				require.Equal(t, "reply from Google", gjson.Get(response, "content.0.text").String())
			}
		})
	}
}

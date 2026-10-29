//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGatewayCapacityDiagnosis_CompatibilityRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []struct {
		name, path, platform, model, body string
		call                              func(*GatewayHandler, *gin.Context)
	}{
		{"messages-gemini", "/v1/messages", service.PlatformGemini, "gemini-4-argon-high",
			`{"model":%q,"stream":%t,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`, (*GatewayHandler).Messages},
		{"messages-antigravity", "/v1/messages", service.PlatformAntigravity, "claude-sonnet-4-5",
			`{"model":%q,"stream":%t,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`, (*GatewayHandler).Messages},
		{"chat-completions", "/v1/chat/completions", service.PlatformAntigravity, "gemini-4-argon-high",
			`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hello"}]}`, (*GatewayHandler).ChatCompletions},
		{"responses", "/v1/responses", service.PlatformAntigravity, "gemini-4-argon-high",
			`{"model":%q,"stream":%t,"input":"hello"}`, (*GatewayHandler).Responses},
	} {
		for _, scenario := range []string{"all supporting accounts cooling down", "empty pool", "unsupported model"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", route.name, scenario, stream), func(t *testing.T) {
					groupID := int64(52)
					group := &service.Group{ID: groupID, Hydrated: true, Platform: route.platform, Status: service.StatusActive}
					model := route.model
					if scenario == "unsupported model" {
						model = "unconfigured-model"
					}
					now := time.Now().UTC().Truncate(time.Second)
					var accounts []*service.Account
					var persisted []service.Account
					if scenario != "empty pool" {
						for i, id := range []int64{288, 296} {
							resetAt := now.Add(time.Duration(i+1) * 90 * time.Second)
							scope := route.model
							if strings.HasPrefix(scope, "gemini-") {
								scope = "antigravity:gemini"
							}
							account := &service.Account{
								ID: id, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
								Status: service.StatusActive, Schedulable: true, Concurrency: 4,
								AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}},
								Credentials:   map[string]any{"model_mapping": map[string]any{route.model: route.model}},
								Extra: map[string]any{
									"mixed_scheduling": true,
									"model_rate_limits": map[string]any{
										scope: map[string]any{"rate_limit_reset_at": resetAt.Format(time.RFC3339)},
									},
								},
							}
							accounts = append(accounts, account)
							persisted = append(persisted, *account)
						}
					}
					h, cleanup := newTestGatewayHandler(t, group, accounts)
					t.Cleanup(cleanup)
					repo := &gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{groupID: persisted}}
					snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
					h.gatewayService = service.NewGatewayService(
						repo, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
						snapshot, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
					)

					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					ctx := context.WithValue(context.Background(), ctxkey.Group, group)
					c.Request = httptest.NewRequest(http.MethodPost, route.path,
						bytes.NewBufferString(fmt.Sprintf(route.body, model, stream))).WithContext(ctx)
					c.Request.Header.Set("Content-Type", "application/json")
					c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
						ID: 1, UserID: 2, GroupID: &groupID, Group: group, Status: service.StatusActive,
						User: &service.User{ID: 2, Concurrency: 10, Balance: 100},
					})
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 10})

					route.call(h, c)

					body := recorder.Body.String()
					require.True(t, gjson.Valid(body), body)
					message := gjson.Get(body, "error.message").String()
					wantType := "api_error"
					switch scenario {
					case "all supporting accounts cooling down":
						require.Equal(t, http.StatusServiceUnavailable, recorder.Code, body)
						require.Equal(t, "model_capacity_exhausted", gjson.Get(body, "error.code").String())
						require.Contains(t, message, model)
						require.Contains(t, message, "Earliest retry time")
						require.NotContains(t, message, "quota reset")
						require.NotContains(t, message, "No available accounts:")
						retryAfter, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
						require.NoError(t, err)
						require.InDelta(t, 90, retryAfter, 2)
					case "empty pool":
						require.Equal(t, http.StatusServiceUnavailable, recorder.Code, body)
						require.Contains(t, message, "No available accounts:")
						require.Empty(t, recorder.Header().Get("Retry-After"))
						if route.name == "responses" {
							require.Equal(t, "api_error", gjson.Get(body, "error.code").String())
						} else {
							require.False(t, gjson.Get(body, "error.code").Exists(), "ordinary errors retain their existing wire format")
						}
					case "unsupported model":
						wantType = "model_not_found"
						require.Equal(t, http.StatusNotFound, recorder.Code, body)
						require.Equal(t, "model_not_found", gjson.Get(body, "error.code").String())
						require.Contains(t, message, "not supported")
						require.Empty(t, recorder.Header().Get("Retry-After"))
					}
					if route.name == "responses" {
						require.False(t, gjson.Get(body, "error.type").Exists(), "Responses retains its code/message envelope")
					} else {
						require.Equal(t, wantType, gjson.Get(body, "error.type").String())
					}
					if strings.HasPrefix(route.name, "messages-") {
						require.Equal(t, "error", gjson.Get(body, "type").String())
					} else {
						require.False(t, gjson.Get(body, "type").Exists())
					}
				})
			}
		}
	}
}

//go:build unit

package handler

import (
	"bytes"
	"context"
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

func TestGeminiV1BetaModels_ModelScopeCapacityDiagnosis(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name           string
		model          string
		secondOverload bool
		wantStatus     int
		wantRetryAfter bool
	}{
		{name: "scope-only quota exhaustion", model: "gemini-4-argon-high", wantStatus: http.StatusServiceUnavailable, wantRetryAfter: true},
		{name: "other account overloaded instead of quota limited", model: "gemini-4-argon-high", secondOverload: true, wantStatus: http.StatusServiceUnavailable},
		{name: "unconfigured model remains not found", model: "gemini-unconfigured", wantStatus: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(52)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAntigravity, Status: service.StatusActive}
			now := time.Now().UTC().Truncate(time.Second)
			var accounts []*service.Account
			var persisted []service.Account
			for i, id := range []int64{288, 296} {
				resetAt := now.Add(time.Duration(i+1) * 90 * time.Second)
				account := &service.Account{
					ID: id, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
					Status: service.StatusActive, Schedulable: true, Concurrency: 4,
					AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}},
					Credentials: map[string]any{"model_mapping": map[string]any{
						"gemini-4-argon-high": "gemini-4-argon-high",
					}},
					Extra: map[string]any{"model_rate_limits": map[string]any{
						"antigravity:gemini": map[string]any{"rate_limit_reset_at": resetAt.Format(time.RFC3339)},
					}},
				}
				if i == 1 && tc.secondOverload {
					account.Extra = nil
					account.OverloadUntil = &resetAt
				}
				accounts = append(accounts, account)
				persisted = append(persisted, *account)
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
			ctx = context.WithValue(ctx, ctxkey.ForcePlatform, service.PlatformAntigravity)
			c.Request = httptest.NewRequest(http.MethodPost, "/antigravity/v1beta/models/"+tc.model+":streamGenerateContent?alt=sse",
				bytes.NewBufferString(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "modelAction", Value: "/" + tc.model + ":streamGenerateContent"}}
			c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformAntigravity)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
				ID: 1, UserID: 2, GroupID: &groupID, Group: group, Status: service.StatusActive,
				User: &service.User{ID: 2, Concurrency: 10, Balance: 100},
			})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 10})

			h.GeminiV1BetaModels(c)

			require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			if tc.wantRetryAfter {
				retryAfter, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
				require.NoError(t, err)
				require.InDelta(t, 90, retryAfter, 2)
				require.Contains(t, recorder.Body.String(), "Earliest retry time", "the real native handler must retain the diagnosis instead of overwriting it with a generic selection error")
			} else {
				require.Empty(t, recorder.Header().Get("Retry-After"))
			}
		})
	}
}

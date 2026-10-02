//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPrismFailoverAvailable(t *testing.T) {
	groupID := int64(39)
	for _, scenario := range []string{"available", "excluded", "rate_limited", "unsupported", "group_unsupported", "disabled", "only_current", "query_failed"} {
		t.Run(scenario, func(t *testing.T) {
			current := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
			other := current
			other.ID = 2
			excluded := map[int64]struct{}{}
			switch scenario {
			case "excluded":
				excluded[2] = struct{}{}
			case "rate_limited":
				until := time.Now().Add(time.Minute)
				other.RateLimitResetAt = &until
			case "unsupported":
				other.Credentials = map[string]any{"model_mapping": map[string]any{"other-model": "other-model"}}
			case "group_unsupported":
				other.AccountGroups = []AccountGroup{{GroupID: groupID, AllowedModels: []string{"other-model"}}}
			case "disabled":
				other.Schedulable = false
			}
			repo := &mockAccountRepoForPlatform{accounts: []Account{current, other}}
			if scenario == "only_current" {
				repo.accounts = []Account{current}
			}
			if scenario == "query_failed" {
				repo.listPlatformFunc = func(context.Context, string) ([]Account, error) { return nil, errors.New("unavailable") }
			}
			svc := &OpenAIGatewayService{accountRepo: repo, cfg: testConfig()}
			require.Equal(t, scenario == "available", svc.PrismFailoverAvailable(context.Background(), &groupID, "gpt-6-astra", 1, excluded))
		})
	}
}

func TestPrismFailoverUpstreamHeaders(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, managed := range []bool{false, true} {
			for _, available := range []bool{false, true} {
				harness := newPrismGatewayHarness(t, nil, nil)
				account := harness.account
				if !managed {
					account = &Account{ID: 200, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				}
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				ctx.Request.Header.Set("X-Prism-Failover", "client-controlled")
				ctx.Set(prismFailoverAvailableKey, available)
				var request *http.Request
				var err error
				if passthrough {
					request, err = harness.svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), ctx, account, []byte(`{"model":"gpt-6-astra"}`), "token")
				} else {
					request, err = harness.svc.buildUpstreamRequest(context.Background(), ctx, account, []byte(`{"model":"gpt-6-astra"}`), "token", false, "", false)
				}
				require.NoError(t, err)
				expected := ""
				if managed {
					expected = "none"
					if available {
						expected = "available"
					}
				}
				require.Equal(t, expected, request.Header.Get("X-Prism-Failover"))
			}
		}
	}
}

func TestPrismFailoverHeadersForChatAndMessages(t *testing.T) {
	for _, family := range []string{"chat", "messages"} {
		t.Run(family, func(t *testing.T) {
			harness := newPrismGatewayHarness(t, nil, nil)
			_, _, err := harness.forward(family, "/v1/"+family, []byte(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`), http.Header{"X-Prism-Failover": []string{"client-controlled"}})
			require.NoError(t, err)
			require.Equal(t, "available", harness.upstream.lastReq.Header.Get("X-Prism-Failover"))
		})
	}
}

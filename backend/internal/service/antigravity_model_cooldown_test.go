//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func antigravityCooldownTestAccount(model string) *Account {
	return &Account{
		ID: 91, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{
			"public-model": model, "gemini-3.8-flash-high": "gemini-3.8-flash-high",
			"gemini-4-argon-high": "gemini-4-argon-high",
		}},
	}
}

func TestAntigravityModelCooldown_429DoesNotDisableOtherGeminiModels(t *testing.T) {
	const flash = "gemini-3.8-flash-high"
	const argon = "gemini-4-argon-high"
	for _, tc := range []struct {
		name, model, otherModel, body string
		minimum                       time.Duration
	}{
		{"generic", flash, argon, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Resource has been exhausted (e.g. check quota)."}}`, 20 * time.Second},
		{"rate without retry", flash, argon, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-3.8-flash-high"}}]}}`, 20 * time.Second},
		{"retry info only", flash, argon, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"60s"}]}}`, 50 * time.Second},
		{"argon quota with short retry first", argon, flash, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.1s"},{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-4-argon-high","quotaResetDelay":"72h"}}]}}`, 71 * time.Hour},
		{"quota metadata without rate reason", flash, argon, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"metadata":{"quotaResetDelay":"24h"}}]}}`, 23 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(antigravityFallbackSecondsEnv, "30")
			repo := &stubAntigravityAccountRepo{}
			cache := &stubSchedulerCache{}
			account := antigravityCooldownTestAccount(tc.model)
			calls := 0
			svc := &AntigravityGatewayService{
				accountRepo:       repo,
				schedulerSnapshot: &SchedulerSnapshotService{cache: cache},
			}
			upstream := &antigravityBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}}

			result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
				ctx: context.Background(), account: account, accountRepo: repo,
				httpUpstream: upstream, accessToken: "test-token", action: "generateContent",
				body: []byte(`{}`), requestedModel: "public-model", handleError: svc.handleUpstreamError,
			})
			if err != nil {
				_, ok := IsAntigravityAccountSwitchError(err)
				require.True(t, ok, "unexpected retry failure: %v", err)
			} else {
				require.NotNil(t, result)
				require.Equal(t, http.StatusTooManyRequests, result.resp.StatusCode)
				require.NoError(t, result.resp.Body.Close())
			}
			require.Equal(t, 1, calls)
			require.Len(t, repo.modelRateLimitCalls, 1)
			require.Equal(t, tc.model, repo.modelRateLimitCalls[0].modelKey)
			require.Greater(t, time.Until(repo.modelRateLimitCalls[0].resetAt), tc.minimum)
			require.False(t, account.IsSchedulableForModel("public-model"))
			require.True(t, account.IsSchedulableForModel(tc.otherModel), "an unrelated Gemini bucket must remain eligible")
			require.Nil(t, account.modelRateLimitResetAt(antigravityGeminiModelRateLimitKey))
			require.Empty(t, cache.setAccountCalls, "do not republish a pre-write account over the repository's fresh snapshot")
		})
	}
}

func TestAntigravityModelCooldown_PreservesExistingHardQuotaAndLegacyScope(t *testing.T) {
	const model = "gemini-3.8-flash-high"
	account := antigravityCooldownTestAccount(model)
	resetAt := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	setAccountModelRateLimitSnapshot(account, model, resetAt, "quota_exhausted", time.Now())
	setAccountModelRateLimitSnapshot(account, antigravityGeminiModelRateLimitKey, resetAt, "quota_exhausted", time.Now())
	repo := &stubAntigravityAccountRepo{}
	svc := &AntigravityGatewayService{accountRepo: repo}

	svc.handleUpstreamError(context.Background(), "[test]", account, 429, http.Header{},
		[]byte(`{"error":{"message":"Resource has been exhausted"}}`), "public-model", 0, "", false)

	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, resetAt, repo.modelRateLimitCalls[0].resetAt)
	require.Equal(t, resetAt, *account.modelRateLimitResetAt(model))
	require.Equal(t, "quota_exhausted", account.modelRateLimitReason(model))
	require.Equal(t, resetAt, *account.modelRateLimitResetAt(antigravityGeminiModelRateLimitKey))
	require.False(t, account.IsSchedulableForModel("gemini-4-argon-high"), "existing shared hard quotas must remain enforced")
}

func TestAntigravityModelCooldown_ErrorPolicyPreservesQuotaReset(t *testing.T) {
	body := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.1s"},{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-4-argon-high","quotaResetDelay":"72h"}}]}}`)
	repo := &stubAntigravityAccountRepo{}
	svc := &AntigravityGatewayService{accountRepo: repo}
	account := antigravityCooldownTestAccount("gemini-4-argon-high")

	handled := svc.handleAntigravityModelRateLimitBeforePolicy(antigravityRetryLoopParams{
		ctx: context.Background(), account: account, accountRepo: repo, requestedModel: "public-model",
	}, 429, http.Header{}, body)

	require.True(t, handled)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gemini-4-argon-high", repo.modelRateLimitCalls[0].modelKey)
	require.Greater(t, time.Until(repo.modelRateLimitCalls[0].resetAt), 71*time.Hour)
	require.True(t, account.IsSchedulableForModel("gemini-3.8-flash-high"))
}

func TestAntigravityModelCooldown_SmartRetryKeepsLastQuotaReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		initial := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-3.8-flash-high"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.1s"}]}}`)
		last := `{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.1s"},{"metadata":{"quotaResetDelay":"72h"}}]}}`
		repo := &stubAntigravityAccountRepo{}
		account := antigravityCooldownTestAccount("gemini-3.8-flash-high")
		svc := &AntigravityGatewayService{accountRepo: repo}
		upstream := &antigravityBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(last))}, nil
		}}

		result := svc.handleSmartRetry(antigravityRetryLoopParams{
			ctx: context.Background(), account: account, accountRepo: repo,
			httpUpstream: upstream, action: "generateContent", body: []byte(`{}`),
			requestedModel: "public-model", handleError: svc.handleUpstreamError,
		}, &http.Response{StatusCode: 429, Header: http.Header{}}, initial, "https://example.test", 0, []string{"https://example.test"})

		require.NotNil(t, result.switchError)
		require.Len(t, repo.modelRateLimitCalls, 1)
		require.Greater(t, time.Until(repo.modelRateLimitCalls[0].resetAt), 71*time.Hour)
		require.True(t, account.IsSchedulableForModel("gemini-4-argon-high"))
	})
}

func TestAntigravityModelCooldown_BareModelUsesForwardedThinkingVariant(t *testing.T) {
	const bare = "gemini-3.8-flash"
	for _, tc := range []struct {
		name, protocol, body, variant string
	}{
		{"native default", "gemini", `{"contents":[{"parts":[{"text":"hi"}]}]}`, "high"},
		{"native low", "gemini", `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`, "low"},
		{"native medium budget", "gemini", `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":4096}}}`, "medium"},
		{"messages disabled", "messages", `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`, "low"},
		{"messages medium", "messages", `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":4096}}`, "medium"},
		{"chat low converts to default high", "chat_completions", `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`, "high"},
		{"chat nested reasoning wins", "chat_completions", `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","reasoning":{"effort":"medium"}}`, "medium"},
		{"responses low converts to default high", "responses", `{"model":"gemini-3.8-flash","input":"hi","reasoning":{"effort":"low"}}`, "high"},
		{"responses medium", "responses", `{"model":"gemini-3.8-flash","input":"hi","reasoning":{"effort":"medium"}}`, "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := newAntigravityAccountWithMapping(map[string]string{
				bare: bare, bare + "-high": bare + "-high",
				bare + "-medium": bare + "-medium", bare + "-low": bare + "-low",
			})
			account.Type = AccountTypeOAuth
			account.Status = StatusActive
			account.Schedulable = true
			account.Credentials["access_token"] = "test-token"
			account.Credentials["project_id"] = "test-project"
			repo := &stubAntigravityAccountRepo{}
			calls := 0
			upstream := &antigravityBudgetUpstream{do: func(request *http.Request) (*http.Response, error) {
				calls++
				var payload struct {
					Model string `json:"model"`
				}
				require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
				require.Equal(t, bare+"-"+tc.variant, payload.Model)
				return &http.Response{
					StatusCode: 429, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`)),
				}, nil
			}}
			svc := &AntigravityGatewayService{
				accountRepo: repo, tokenProvider: &AntigravityTokenProvider{}, httpUpstream: upstream,
				settingService: NewSettingService(&antigravitySettingRepoStub{}, &config.Config{}),
			}
			body := []byte(tc.body)
			forward := func() error {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
				// Direct service callers do not have the handler's thinking context.
				// Forwarding must still use its concrete variant for cooldown writes.
				var err error
				switch tc.protocol {
				case "gemini":
					_, err = svc.ForwardGemini(context.Background(), c, account, bare, "generateContent", false, body, false)
				case "messages":
					_, err = svc.Forward(context.Background(), c, account, body, false)
				case "chat_completions":
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
				case "responses":
					_, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
				}
				return err
			}
			var failover *UpstreamFailoverError
			require.ErrorAs(t, forward(), &failover)
			require.Equal(t, http.StatusTooManyRequests, failover.StatusCode)
			require.NotEmpty(t, repo.modelRateLimitCalls)
			for _, write := range repo.modelRateLimitCalls {
				require.Equal(t, bare+"-"+tc.variant, write.modelKey)
			}

			ctx := WithAntigravityThinkingFromRequest(context.Background(), body, tc.protocol)
			require.False(t, account.IsSchedulableForModelWithContext(ctx, bare), "selection must read the same key that forwarding wrote")
			otherLevel := "high"
			if tc.variant == otherLevel {
				otherLevel = "low"
			}
			other := WithAntigravityThinkingFromRequest(context.Background(), []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"`+otherLevel+`"}}}`), "gemini")
			require.True(t, account.IsSchedulableForModelWithContext(other, bare), "another variant must remain available")
			require.ErrorAs(t, forward(), &failover)
			require.Equal(t, 1, calls, "forward precheck must also enforce the same variant cooldown")
		})
	}
}

func TestAntigravityModelCooldown_VariantOnlyAndAliasMapping(t *testing.T) {
	const bare = "gemini-3.5-flash"
	account := newAntigravityAccountWithMapping(map[string]string{
		bare + "-low": bare + "-low", bare + "-high": bare + "-high",
		"client-alias": bare + "-high",
	})
	account.Type = AccountTypeOAuth
	// This model is not among the automatically added default passthroughs.
	_, hasBare := account.GetModelMapping()[bare]
	require.False(t, hasBare)
	gateway := &GatewayService{}
	ctx := WithAntigravityThinkingFromRequest(nil, []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`), "gemini")
	require.True(t, gateway.isModelSupportedByAccountWithContext(ctx, account, bare))
	require.True(t, gateway.isModelSupportedByAccount(account, bare))
	require.Equal(t, bare+"-low", resolveFinalAntigravityModelKey(ctx, account, bare))
	require.Equal(t, bare+"-high", resolveFinalAntigravityModelKey(context.Background(), account, bare))

	// An explicit alias is resolved exactly once, even if its target is also a
	// public mapping key pointing elsewhere.
	aliasAccount := newAntigravityAccountWithMapping(map[string]string{
		"client-alias": bare + "-high", bare + "-high": "gemini-4-argon-high",
		bare: "gemini-4-argon-high", bare + "-low": bare + "-low",
	})
	aliasAccount.Type = AccountTypeOAuth
	require.Equal(t, bare+"-high", resolveFinalAntigravityModelKey(ctx, aliasAccount, "client-alias"))
	require.Equal(t, "gemini-4-argon-high", resolveFinalAntigravityModelKey(ctx, aliasAccount, bare))

	// The context does not change any other provider's model or cooldown keys.
	other := &Account{Platform: PlatformOpenAI, Credentials: map[string]any{
		"model_mapping": map[string]any{bare: bare, bare + "-low": bare + "-low"},
	}}
	require.Equal(t, []string{bare}, other.modelRateLimitKeysForRequest(ctx, bare))
}

func TestAntigravityModelCooldown_StickySessionUsesRequestedVariant(t *testing.T) {
	const bare = "gemini-3.8-flash"
	account := newAntigravityAccountWithMapping(map[string]string{
		bare + "-high": bare + "-high", bare + "-low": bare + "-low",
	})
	account.Type = AccountTypeOAuth
	account.Status = StatusActive
	account.Schedulable = true
	setAccountModelRateLimitSnapshot(account, bare+"-high", time.Now().Add(time.Minute), "rate_limited", time.Now())
	low := WithAntigravityThinkingFromRequest(context.Background(), []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`), "gemini")
	require.True(t, account.IsSchedulableForModelWithContext(low, bare))
	require.False(t, shouldClearStickySessionWithContext(low, account, bare), "high's cooldown must not evict a usable low session")
	require.True(t, shouldClearStickySessionWithContext(context.Background(), account, bare), "default high must still clear the unavailable session")

	setAccountModelRateLimitSnapshot(account, bare+"-low", time.Now().Add(time.Minute), "rate_limited", time.Now())
	require.True(t, shouldClearStickySessionWithContext(low, account, bare))
}

func TestAntigravityModelCooldown_PassthroughAccountKeepsBareModel(t *testing.T) {
	const bare = "gemini-3.8-flash"
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeUpstream} {
		t.Run(accountType, func(t *testing.T) {
			account := newAntigravityAccountWithMapping(map[string]string{
				bare: bare, bare + "-high": bare + "-high", bare + "-low": bare + "-low",
			})
			account.Type = accountType
			account.Status = StatusActive
			account.Schedulable = true
			ctx := WithAntigravityThinkingFromRequest(context.Background(), []byte(`{}`), "gemini")
			require.Equal(t, bare, resolveFinalAntigravityModelKey(ctx, account, bare))
			setAccountModelRateLimitSnapshot(account, bare+"-high", time.Now().Add(time.Minute), "rate_limited", time.Now())
			require.True(t, account.IsSchedulableForModelWithContext(ctx, bare))
			require.False(t, shouldClearStickySessionWithContext(ctx, account, bare))
			setAccountModelRateLimitSnapshot(account, bare, time.Now().Add(time.Minute), "rate_limited", time.Now())
			require.False(t, account.IsSchedulableForModelWithContext(ctx, bare))
		})
	}
}

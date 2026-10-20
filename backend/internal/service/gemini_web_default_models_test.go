//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/gemini"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

var expectedGeminiWebDefaultMappings = map[string]string{
	"gemini-4-argon":        "gemini-4-argon",
	"gemini-3.8-flash":      "gemini-3.8-flash",
	"gemini-3.5-flash-lite": "gemini-3.5-flash-lite",
	"gemini-3.8-flash-high": "gemini-3.8-flash",
	"gemini-4-argon-high":   "gemini-4-argon",
	"gemini-4-argon-low":    "gemini-4-argon",
	"gemini-4-argon-medium": "gemini-4-argon",
}

func TestGeminiWebDefaultModelMappingSupportsOnlyPublicDefaults(t *testing.T) {
	for _, credentials := range []map[string]any{nil, {}, {"model_mapping": map[string]any{}}} {
		account := &Account{Platform: PlatformGemini, Type: AccountTypeGeminiWeb, Credentials: credentials}
		mapping := account.GetModelMapping()
		require.Equal(t, expectedGeminiWebDefaultMappings, mapping)
		for public, upstream := range expectedGeminiWebDefaultMappings {
			require.True(t, account.IsModelSupported(public), public)
			require.True(t, (&GatewayService{}).isModelSupportedByAccount(account, public), public)
			require.True(t, (&GeminiMessagesCompatService{}).isModelSupportedByAccount(account, public), public)
			require.Equal(t, upstream, account.GetMappedModel(public))
			_, known := geminiweb.LookupModel(upstream)
			require.True(t, known, "default target must have a Web protocol model: %s", upstream)
		}
		for _, model := range []string{"gemini-3.1-pro", "gemini-auto", "gemini-3.8-flash-low", "gemini-3.8-flash-thinking", "gemini-2.5-pro", "claude-sonnet-4-6", "unknown"} {
			require.False(t, account.IsModelSupported(model), model)
			require.False(t, (&GeminiMessagesCompatService{}).isModelSupportedByAccount(account, model), model)
		}
	}
}

func TestGeminiWebDefaultModelMappingPreservesExplicitRules(t *testing.T) {
	account := geminiWebTestAccount()
	// Populate the model cache before replacing the defaults with an explicit scope.
	require.True(t, account.IsModelSupported("gemini-4-argon"))
	account.Credentials["model_mapping"] = map[string]any{
		"custom-pro":            "gemini-3.1-pro",
		"gemini-3.8-flash-high": "gemini-3.5-flash-lite",
	}
	require.Len(t, account.GetModelMapping(), 2)
	require.Equal(t, "gemini-3.1-pro", account.GetMappedModel("custom-pro"))
	require.Equal(t, "gemini-3.5-flash-lite", account.GetMappedModel("gemini-3.8-flash-high"))
	require.True(t, account.IsModelSupported("custom-pro"))
	require.False(t, account.IsModelSupported("gemini-3.1-pro"), "a target alone does not allow direct requests")
	require.False(t, account.IsModelSupported("gemini-4-argon"), "explicit scopes must not inherit defaults")
	delete(account.Credentials, "model_mapping")
	require.True(t, account.IsModelSupported("gemini-4-argon"), "clearing explicit rules restores defaults")
}

func TestGeminiWebDefaultModelMappingDoesNotChangeOtherAccountTypes(t *testing.T) {
	for _, account := range []*Account{
		{Platform: PlatformGemini, Type: AccountTypeAPIKey},
		{Platform: PlatformGemini, Type: AccountTypeOAuth},
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
	} {
		require.Empty(t, account.GetModelMapping(), "%s/%s", account.Platform, account.Type)
		require.Equal(t, "gemini-4-argon-low", account.GetMappedModel("gemini-4-argon-low"))
	}
	ag := &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth}
	require.Equal(t, "gemini-4-argon-low", ag.GetMappedModel("gemini-4-argon-low"))
	require.True(t, ag.IsModelSupported("claude-sonnet-4-6"))
}

type geminiWebDefaultModelsRepo struct {
	AccountRepository
	accounts []Account
}

func (r *geminiWebDefaultModelsRepo) Create(_ context.Context, account *Account) error {
	account.ID = int64(len(r.accounts) + 1)
	r.accounts = append(r.accounts, *account)
	return nil
}

func (r *geminiWebDefaultModelsRepo) ListSchedulableByGroupID(context.Context, int64) ([]Account, error) {
	return r.accounts, nil
}

func TestGeminiWebDefaultModelMappingAppliesToCreationPaths(t *testing.T) {
	for _, entrypoint := range []string{"admin", "account-service"} {
		t.Run(entrypoint, func(t *testing.T) {
			repo := &geminiWebDefaultModelsRepo{}
			credentials := map[string]any{"gemini_web_cookie": "__Secure-1PSID=test-default-models-cookie"}
			var account *Account
			var err error
			if entrypoint == "admin" {
				account, err = (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
					Name: "web-defaults", Platform: PlatformGemini, Type: AccountTypeGeminiWeb,
					Credentials: credentials, SkipDefaultGroupBind: true,
				})
			} else {
				account, err = (&AccountService{accountRepo: repo}).Create(context.Background(), CreateAccountRequest{
					Name: "web-defaults", Platform: PlatformGemini, Type: AccountTypeGeminiWeb, Credentials: credentials,
				})
			}
			require.NoError(t, err)
			require.Equal(t, geminiweb.DefaultModelMapping(), account.GetModelMapping())
			require.NotContains(t, account.Credentials, "model_mapping", "runtime defaults do not overwrite stored configuration")
		})
	}
}

func TestGeminiWebDefaultModelsCatalogsMatchEffectiveRules(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			account := geminiWebTestAccount()
			if explicit {
				account.Credentials["model_mapping"] = map[string]any{"custom-pro": "gemini-3.1-pro"}
			}
			want := make([]string, 0, len(account.GetModelMapping()))
			for public := range account.GetModelMapping() {
				want = append(want, public)
			}
			groupID := int64(12)
			gateway := &GatewayService{accountRepo: &geminiWebDefaultModelsRepo{accounts: []Account{*account}}}
			require.ElementsMatch(t, want, gateway.GetAvailableModels(context.Background(), &groupID, PlatformGemini))
			svc := &GeminiMessagesCompatService{}
			list, err := svc.ForwardListModels(context.Background(), account)
			require.NoError(t, err)
			wantNames := make([]string, 0, len(want))
			for _, public := range want {
				wantNames = append(wantNames, "models/"+public)
				result, err := svc.ForwardGetModel(context.Background(), account, public)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, result.StatusCode)
				var model gemini.Model
				require.NoError(t, json.Unmarshal(result.Body, &model))
				require.Equal(t, "models/"+public, model.Name, "keep the public alias in model metadata")
			}
			require.ElementsMatch(t, wantNames, decodeGeminiModelNames(t, list.Body))
			result, err := svc.ForwardGetModel(context.Background(), account, "gemini-3.1-pro")
			require.NoError(t, err)
			require.Equal(t, http.StatusNotFound, result.StatusCode, "a technical catalog entry is not automatically enabled")
		})
	}
}

func TestGeminiWebDefaultModelsNativeCatalogExpandsExplicitWildcard(t *testing.T) {
	account := geminiWebTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"gemini-3.8-*": "gemini-3.8-flash"}
	result, err := (&GeminiMessagesCompatService{}).ForwardListModels(context.Background(), account)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		"models/gemini-3.8-flash", "models/gemini-3.8-flash-thinking", "models/gemini-3.8-flash-thinking-lite",
	}, decodeGeminiModelNames(t, result.Body))
}

func TestGeminiWebDefaultAliasesReachCanonicalUpstreamAcrossProtocols(t *testing.T) {
	useGeminiWebDiagnosticsDir(t)
	useGeminiWebAnalyticsDir(t)
	gin.SetMode(gin.TestMode)
	for public, canonical := range expectedGeminiWebDefaultMappings {
		if public == canonical {
			continue
		}
		for _, protocol := range []string{"native", "claude", "chat"} {
			t.Run(public+"/"+protocol, func(t *testing.T) {
				frames, _ := geminiWebFramesStub(t, "Hello from the requested model.")
				var generationHeader string
				upstream := &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodPost {
						generationHeader = req.Header.Get("x-goog-ext-525001261-jspb")
					}
					return frames.do(req)
				}}
				svc := &GeminiMessagesCompatService{httpUpstream: upstream}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
				var result *ForwardResult
				var err error
				account := geminiWebTestAccount()
				switch protocol {
				case "native":
					result, err = svc.ForwardNative(context.Background(), c, account, public, "generateContent", false,
						[]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
				case "claude":
					result, err = svc.Forward(context.Background(), c, account,
						[]byte(fmt.Sprintf(`{"model":%q,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`, public)))
				case "chat":
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, account,
						[]byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, public)))
				}
				require.NoError(t, err)
				require.Equal(t, canonical, result.UpstreamModel)
				require.Equal(t, public, result.Model)
				expected, known := geminiweb.LookupModel(canonical)
				require.True(t, known)
				require.Contains(t, generationHeader, expected.HeaderID, "actual Web request must use the canonical selector")
			})
		}
	}
}

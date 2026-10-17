//go:build unit

package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAccountTestModelVerification(t *testing.T) {
	for _, tc := range []struct {
		sent, actual, want string
		web, conflict      bool
	}{
		{"gemini-4-argon", "gemini-3.5-flash-lite", "mismatch", true, false},
		{"gemini-4-argon", "gemini-4-argon", "verified", true, false},
		{"gemini-pro", "gemini-3.1-pro", "verified", true, false},
		{"gemini-auto", "gemini-3.5-flash-lite", "verified", true, false},
		{"gemini-3.8-flash-thinking", "gemini-3.8-flash", "mismatch", true, false},
		{"gemini-4-argon", "", "mismatch", true, false},
		{"gemini-auto", "", "mismatch", true, false},
		{"gemini-4-argon", "gemini-4-argon", "mismatch", true, true},
		{"gemini-4-argon-high", "gemini-3.5-flash-lite", "mismatch", false, false},
		{"gemini-4-argon-high", "models/gemini-4-argon", "verified", false, false},
		{"gemini-4-argon-high", "gemini-4-argon-low", "mismatch", false, false},
		{"gemini-3.8-flash-high", "gemini-3.8-flash", "verified", false, false},
		{"gemini-3.8-flash", "gemini-3.5-flash-lite", "mismatch", false, false},
		{"gemini-4-argon-high", "", "unverified", false, false},
	} {
		t.Run(tc.sent+"/"+tc.actual+"/"+tc.want, func(t *testing.T) {
			status, detail := accountTestModelVerification(tc.sent, tc.actual, tc.conflict, tc.web)
			require.Equal(t, tc.want, status)
			if status != "verified" {
				require.NotEmpty(t, detail)
			}
		})
	}
}

func TestAntigravityAccountTestRejectsWrongResponseModel(t *testing.T) {
	for _, actual := range []string{"gemini-4-argon", "gemini-3.5-flash-lite", ""} {
		t.Run(actual, func(t *testing.T) {
			body := `data: {"response":{"modelVersion":"` + actual + `","candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}}` + "\n\n"
			upstream := &geminiCompatHTTPUpstreamStub{response: newJSONResponse(http.StatusOK, body)}
			gateway := newAntigravityCompatService(config.GatewayConfig{}, upstream)
			account := newAntigravityCompatAccount(AccountTypeOAuth)
			account.Credentials["model_mapping"] = map[string]any{"public-alias": "gemini-4-argon-high"}
			svc := &AccountTestService{antigravityGatewayService: gateway}
			c, rec := newTestContext()
			c.Set(accountTestVerifyModelContextKey, true)
			err := svc.testAntigravityAccountConnection(c, account, "public-alias", "")
			if actual == "gemini-4-argon" {
				require.NoError(t, err)
				require.Contains(t, rec.Body.String(), `"status":"verified"`)
				require.Contains(t, rec.Body.String(), `"success":true`)
			} else {
				require.Error(t, err)
				require.NotContains(t, rec.Body.String(), `"success":true`)
				status := "mismatch"
				if actual == "" {
					status = "unverified"
				}
				require.Contains(t, rec.Body.String(), `"status":"`+status+`"`)
			}
			require.Equal(t, 1, upstream.calls)
			require.Contains(t, rec.Body.String(), `"model":"gemini-4-argon-high"`, "compare mapped upstream model, not the public alias")
		})
	}
}

func TestConnectivityProbeDoesNotRequireModelIdentity(t *testing.T) {
	c, rec := newTestContext()
	err := (&AccountTestService{}).finishAccountModelTest(c, "gemini-4-argon", "", false, false)
	require.NoError(t, err, "background connectivity checks retain their existing semantics")
	require.Contains(t, rec.Body.String(), `"status":"unverified"`)
	require.Contains(t, rec.Body.String(), `"success":true`)
}

func TestGeminiWebAccountTestRejectsDowngradeAndUnsupportedModel(t *testing.T) {
	for _, tc := range []struct{ requested, returned, want string }{
		{"gemini-4-argon", "cf41b0e0dd7d53e5", "mismatch"},
		{"gemini-3.5-flash-lite", "cf41b0e0dd7d53e5", "verified"},
		{"gemini-4-argon", "", "mismatch"},
		{"gemini-4-argon-high", "95a4c0abcba24364", "unsupported"},
		{"custom-alias", "cf41b0e0dd7d53e5", "verified"},
	} {
		t.Run(tc.requested+"/"+tc.want, func(t *testing.T) {
			payload := make([]any, 43)
			payload[4] = []any{[]any{"candidate", []any{"OK"}}}
			payload[39] = tc.returned
			b, err := json.Marshal(payload)
			require.NoError(t, err)
			outer, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(b)}})
			require.NoError(t, err)
			upstream := &queuedHTTPUpstream{responses: []*http.Response{
				newJSONResponse(200, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`),
				newJSONResponse(200, string(outer)),
			}}
			svc := &AccountTestService{httpUpstream: upstream}
			account := &Account{Platform: PlatformGemini, Type: AccountTypeGeminiWeb, Credentials: map[string]any{
				"gemini_web_cookie": "__Secure-1PSID=psid",
				"model_mapping":     map[string]any{"custom-alias": "gemini-3.5-flash-lite"},
			}}
			c, rec := newTestContext()
			c.Set(accountTestVerifyModelContextKey, true)
			err = svc.testGeminiWebAccountConnection(c, account, tc.requested, "")
			if tc.want == "verified" {
				require.NoError(t, err)
				require.Contains(t, rec.Body.String(), `"success":true`)
			} else {
				require.Error(t, err)
				require.NotContains(t, rec.Body.String(), `"success":true`)
			}
			if tc.want == "unsupported" {
				require.Empty(t, upstream.requests, "unknown Web model must not fall back to Argon")
			} else {
				require.Contains(t, rec.Body.String(), `"status":"`+tc.want+`"`)
			}
		})
	}
}

func TestGeminiAccountTestReadsTrailingResponseModel(t *testing.T) {
	for _, tc := range []struct{ data, status string }{
		{`data: {"response":{"modelVersion":"gemini-3.5-flash-lite"}}`, "mismatch"},
		{`data: {"modelVersion":"gemini-4-argon"}`, "verified"},
		{`data: {"response":{"response":{"modelVersion":"gemini-4-argon"}}}`, "verified"},
		{"", "unverified"},
		{"data: {\"modelVersion\":\"gemini-4-argon\"}\n\ndata: {\"modelVersion\":\"gemini-3.5-flash-lite\"}", "unverified"},
	} {
		t.Run(tc.data, func(t *testing.T) {
			body := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"OK\"}]},\"finishReason\":\"STOP\"}]}\n\n" + tc.data
			observer := observeGeminiTestResponse([]byte(body))
			status, _ := accountTestModelVerification("gemini-4-argon-high", observer.Model(), observer.Conflict(), false)
			require.Equal(t, tc.status, status, "Antigravity buffered SSE model observation")
			c, rec := newTestContext()
			c.Set(accountTestVerifyModelContextKey, true)
			err := (&AccountTestService{}).processGeminiStreamForModel(c, strings.NewReader(body), "gemini-4-argon-high")
			require.Contains(t, rec.Body.String(), `"status":"`+tc.status+`"`)
			if tc.status == "verified" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.NotContains(t, rec.Body.String(), `"success":true`)
			}
		})
	}
}

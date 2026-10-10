//go:build unit

package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPelicanQuestionUsesOneGenerationOnAdaptiveAccounts(t *testing.T) {
	account := adaptiveCNAccountTestAccount(701, PlatformDeepseek)
	svc, upstream := adaptiveCNAccountTestService(account, adaptiveCNChatTestResponse())
	c, recorder := newTestContext()
	prompt := CandyPrompt + "\n\n只输出最终整数，不要解释。"

	require.NoError(t, svc.TestPelicanAccountConnection(c, account.ID, "deepseek-chat", prompt, "medium"))
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/v1/chat/completions", upstream.requests[0].URL.Path)
	require.Equal(t, prompt, gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
	require.Equal(t, 1, strings.Count(recorder.Body.String(), `"type":"test_complete"`))
}

func TestPelicanQuestionReachesModelRoutedNativeEndpoints(t *testing.T) {
	for _, tc := range []struct {
		model    string
		response func() *http.Response
		path     string
	}{
		{"deepseek-v4-flash", adaptiveCNChatTestResponse, "messages.0.content"},
		{"grok-4.6", adaptiveCNResponsesTestResponse, "input.0.content.0.text"},
		{"minimax-m3", adaptiveCNAnthropicTestResponse, "messages.0.content.0.text"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			account := openCodeGoTestAccount(702)
			svc, upstream := adaptiveCNAccountTestService(account, tc.response())
			c, recorder := newTestContext()
			prompt := CandyPrompt + "\n\n只输出最终整数，不要解释。"

			require.NoError(t, svc.TestPelicanAccountConnection(c, account.ID, tc.model, prompt, "medium"))
			require.Len(t, upstream.requests, 1)
			require.Equal(t, prompt, gjson.GetBytes(upstream.lastBody, tc.path).String())
			require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
		})
	}
}

func TestPelicanBedrockQuestionAndAnswerBlocks(t *testing.T) {
	account := &Account{
		ID: 703, Platform: PlatformAnthropic, Type: AccountTypeBedrock,
		Credentials: map[string]any{
			"auth_mode": "apikey", "api_key": "test-key", "aws_region": "us-east-1",
		},
	}
	svc, upstream := adaptiveCNAccountTestService(account, newJSONResponse(http.StatusOK,
		`{"content":[{"type":"thinking","thinking":"reasoning"},{"type":"text","text":"2"},{"type":"text","text":"1"}]}`))
	c, recorder := newTestContext()

	require.NoError(t, svc.TestPelicanAccountConnection(c, account.ID, "claude-sonnet-4-6", CandyPrompt, "medium"))
	require.Equal(t, CandyPrompt, gjson.GetBytes(upstream.lastBody, "messages.0.content.0.text").String())
	require.Greater(t, gjson.GetBytes(upstream.lastBody, "max_tokens").Int(), int64(256))
	require.Contains(t, recorder.Body.String(), `"text":"21"`)
}

func TestPelicanAntigravityQuestionReachesUpstreamWithoutThinkingText(t *testing.T) {
	for _, model := range []string{"gemini-2.5-flash", "claude-sonnet-4-6"} {
		t.Run(model, func(t *testing.T) {
			account := &Account{
				ID: 704, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "test-token", "project_id": "test-project",
					"model_mapping": map[string]any{model: model},
				},
			}
			svc, upstream := adaptiveCNAccountTestService(account, &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"reasoning","thought":true},{"text":"21"}]},"finishReason":"STOP"}]}}

`)),
			})
			svc.antigravityGatewayService = &AntigravityGatewayService{
				tokenProvider:  &AntigravityTokenProvider{},
				httpUpstream:   upstream,
				settingService: NewSettingService(&antigravitySettingRepoStub{}, svc.cfg),
			}
			c, recorder := newTestContext()

			require.NoError(t, svc.TestPelicanAccountConnection(c, account.ID, model, CandyPrompt, "medium"))
			require.Len(t, upstream.requests, 1)
			require.Equal(t, CandyPrompt, gjson.GetBytes(upstream.lastBody, "request.contents.0.parts.0.text").String())
			require.Greater(t, gjson.GetBytes(upstream.lastBody, "request.generationConfig.maxOutputTokens").Int(), int64(1))
			require.Contains(t, recorder.Body.String(), `"text":"21"`)
			require.NotContains(t, recorder.Body.String(), "reasoning")
		})
	}
}

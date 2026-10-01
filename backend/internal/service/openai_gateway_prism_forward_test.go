//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	prismTestResponsesJSON = `{"id":"resp_prism","object":"response","model":"gpt-6.1-sol","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	prismTestChatJSON      = `{"id":"chatcmpl_prism","object":"chat.completion","model":"gpt-6.1-sol","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	prismTestResponsesSSE  = "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_prism","model":"gpt-6.1-sol","status":"in_progress","output":[]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_prism","object":"response","model":"gpt-6.1-sol","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
)

// prismOrderRecorder notes how many sidecar session requests had happened when
// each upstream call was made, proving the session is ensured first.
type prismOrderRecorder struct {
	*httpUpstreamRecorder
	runtimeCalls           *atomic.Int32
	runtimeCallsAtUpstream []int32
}

func (u *prismOrderRecorder) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	u.runtimeCallsAtUpstream = append(u.runtimeCallsAtUpstream, u.runtimeCalls.Load())
	return u.httpUpstreamRecorder.Do(req, proxyURL, accountID, accountConcurrency)
}

// How the fake sidecar answers the gateway's session checks.
const (
	prismRuntimeReady int32 = iota
	prismRuntimeNotReady
	prismRuntimeFailing
)

type prismGatewayHarness struct {
	svc            *OpenAIGatewayService
	account        *Account
	repo           *prismTestRepository
	upstream       *prismOrderRecorder
	runtime        *atomic.Int32
	runtimeMode    *atomic.Int32
	runtimeServer  *httptest.Server
	ctx            context.Context
	upstreamAnswer *http.Response
}

func newPrismGatewayHarness(t *testing.T, group *Group, settings *OpenAIFastPolicySettings) *prismGatewayHarness {
	t.Helper()
	var runtimeCalls, runtimeMode atomic.Int32
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtimeCalls.Add(1)
		switch runtimeMode.Load() {
		case prismRuntimeFailing:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"account_not_ready"}}`))
		case prismRuntimeNotReady:
			_, _ = w.Write([]byte(`{"phase":"initializing","ready":false,"models":[]}`))
		default:
			_, _ = w.Write([]byte(`{"phase":"ready","ready":true,"models":["gpt-6.1-sol"]}`))
		}
	}))
	t.Cleanup(runtime.Close)
	account := prismTestManaged(runtime.URL)
	account.Schedulable = true
	account.Extra["prism_phase"] = "ready"
	account.Extra["openai_responses_mode"] = "force_responses"
	account.Extra["openai_apikey_responses_websockets_v2_mode"] = "http_bridge"
	account.Credentials["openai_capabilities"] = []string{"chat_completions"}
	account.Credentials["model_mapping"] = map[string]any{"gpt-6.1-sol": "gpt-6.1-sol"}
	repo := newPrismTestRepository(prismTestSource(), account)
	prism := prismTestService(t, runtime.URL, repo, &prismTestTokens{token: prismTestJWT("valid")})
	svc := newOpenAIGatewayServiceWithSettings(t, settings)
	svc.cfg = rawChatCompletionsTestConfig()
	svc.cfg.Security.URLAllowlist.AllowPrivateHosts = true
	svc.accountRepo, svc.prismAccounts = repo, prism
	upstream := &prismOrderRecorder{httpUpstreamRecorder: &httpUpstreamRecorder{}, runtimeCalls: &runtimeCalls}
	svc.httpUpstream = upstream
	if group == nil {
		group = &Group{ID: 10, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}
	}
	return &prismGatewayHarness{svc: svc, account: account, repo: repo, upstream: upstream, runtime: &runtimeCalls,
		runtimeMode: &runtimeMode, runtimeServer: runtime, ctx: context.WithValue(context.Background(), ctxkey.Group, group)}
}

// forward sends one request through the entry point of the given family
// (responses, chat or messages) and returns what the client would have seen.
func (h *prismGatewayHarness) forward(family, path string, body []byte, header http.Header) (*httptest.ResponseRecorder, *gin.Context, error) {
	response, contentType := prismTestResponsesJSON, "application/json"
	switch family {
	case "chat":
		response = prismTestChatJSON
	case "messages":
		response, contentType = prismTestResponsesSSE, "text/event-stream"
	}
	h.upstream.resp = &http.Response{StatusCode: http.StatusOK,
		Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(response))}
	if h.upstreamAnswer != nil {
		h.upstream.resp = h.upstreamAnswer
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for key, values := range header {
		c.Request.Header[key] = values
	}
	var err error
	switch family {
	case "chat":
		_, err = h.svc.ForwardAsChatCompletions(h.ctx, c, h.account, body, "", "")
	case "messages":
		_, err = h.svc.ForwardAsAnthropic(h.ctx, c, h.account, body, "", "")
	default:
		_, err = h.svc.Forward(h.ctx, c, h.account, body)
	}
	return recorder, c, err
}

func (h *prismGatewayHarness) upstreamBody(t *testing.T) []byte {
	t.Helper()
	require.Len(t, h.upstream.bodies, 1, "exactly one upstream call is expected")
	return h.upstream.lastBody
}

func TestPrismGatewayDoesNotInjectUnsupportedFastTier(t *testing.T) {
	for _, family := range []string{"responses", "chat", "messages"} {
		for _, force := range []string{"group", "global_policy"} {
			t.Run(family+"_"+force, func(t *testing.T) {
				settings := DefaultOpenAIFastPolicySettings()
				if force == "global_policy" {
					settings.Rules = []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierMissing,
						Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll}}
				}
				h := newPrismGatewayHarness(t, &Group{ID: 10, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true,
					ForceOpenAIFast: force == "group"}, settings)
				path, body := "/v1/responses", `{"model":"gpt-6.1-sol","input":"hello","stream":false}`
				switch family {
				case "chat":
					path, body = "/v1/chat/completions", `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}],"stream":false}`
				case "messages":
					path, body = "/v1/messages", `{"model":"gpt-6.1-sol","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`
				}
				recorder, _, err := h.forward(family, path, []byte(body), nil)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, recorder.Code)
				upstreamBody := h.upstreamBody(t)
				require.False(t, gjson.GetBytes(upstreamBody, "service_tier").Exists(), "Prism has no Fast tier")
				endpoint := "/v1/responses"
				if family == "chat" {
					endpoint = "/v1/chat/completions"
				}
				require.Equal(t, endpoint, strings.TrimPrefix(h.upstream.lastReq.URL.Path, "/accounts/32"))
			})
		}
	}
}

func TestPrismGatewayDropsExplicitFastTierInsteadOfRejecting(t *testing.T) {
	for _, family := range []string{"responses", "chat", "messages"} {
		t.Run(family, func(t *testing.T) {
			h := newPrismGatewayHarness(t, nil, nil)
			path, body := "/v1/responses", `{"model":"gpt-6.1-sol","input":"hello","service_tier":"priority","stream":false}`
			header := http.Header{}
			switch family {
			case "chat":
				path, body = "/v1/chat/completions", `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}],"service_tier":"priority","stream":false}`
			case "messages":
				path, body = "/v1/messages", `{"model":"gpt-6.1-sol","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`
				header.Set("anthropic-beta", claude.BetaFastMode)
			}
			recorder, _, err := h.forward(family, path, []byte(body), header)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.False(t, gjson.GetBytes(h.upstreamBody(t), "service_tier").Exists(),
				"the hint is dropped so Prism is neither sent nor billed a Priority tier it cannot honor")
		})
	}
}

func TestPrismGatewayForwardsCodexStyleResponsesRequest(t *testing.T) {
	h := newPrismGatewayHarness(t, nil, nil)
	body := `{"model":"gpt-6.1-sol","stream":false,"store":false,"parallel_tool_calls":true,
		"instructions":"You are Codex, a coding agent.","reasoning":{"effort":"medium","summary":"auto"},
		"include":["reasoning.encrypted_content"],"prompt_cache_key":"session-1","temperature":0.2,
		"tools":[{"type":"function","name":"shell","description":"run a command","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},{"type":"web_search"}],
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"one file"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"read it"}]}]}`
	recorder, _, err := h.forward("responses", "/v1/responses", []byte(body), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	upstreamBody := h.upstreamBody(t)
	require.Equal(t, "/v1/responses", strings.TrimPrefix(h.upstream.lastReq.URL.Path, "/accounts/32"))
	require.Equal(t, "You are Codex, a coding agent.", gjson.GetBytes(upstreamBody, "instructions").String())
	require.Equal(t, int64(2), gjson.GetBytes(upstreamBody, "tools.#").Int())
	require.Equal(t, "shell", gjson.GetBytes(upstreamBody, "tools.0.name").String())
	require.Equal(t, int64(5), gjson.GetBytes(upstreamBody, "input.#").Int(), "the whole history reaches the sidecar")
	require.Equal(t, "function_call_output", gjson.GetBytes(upstreamBody, "input.2.type").String())
	require.Equal(t, "read it", gjson.GetBytes(upstreamBody, "input.4.content.0.text").String())
	require.Equal(t, "medium", gjson.GetBytes(upstreamBody, "reasoning.effort").String())
}

func TestPrismGatewayForwardsChatToolsAndHistory(t *testing.T) {
	h := newPrismGatewayHarness(t, nil, nil)
	body := `{"model":"gpt-6.1-sol","stream":false,"reasoning_effort":"high","tool_choice":"auto","temperature":0.2,
		"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],
		"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"find it"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"found"},{"role":"user","content":"thanks"}]}`
	recorder, _, err := h.forward("chat", "/v1/chat/completions", []byte(body), nil)
	require.NoError(t, err, "gpt-6.1-sol's Chat tool limits belong to the official upstream, not to Prism")
	require.Equal(t, http.StatusOK, recorder.Code)
	upstreamBody := h.upstreamBody(t)
	require.Equal(t, "/v1/chat/completions", strings.TrimPrefix(h.upstream.lastReq.URL.Path, "/accounts/32"))
	require.Equal(t, "search", gjson.GetBytes(upstreamBody, "tools.0.function.name").String())
	require.Equal(t, int64(5), gjson.GetBytes(upstreamBody, "messages.#").Int())
	require.Equal(t, "be brief", gjson.GetBytes(upstreamBody, "messages.0.content").String())
	require.Equal(t, "call_1", gjson.GetBytes(upstreamBody, "messages.3.tool_call_id").String())
}

func TestPrismGatewayMessagesKeepsFullHistoryAfterEnsuringTheSession(t *testing.T) {
	h := newPrismGatewayHarness(t, nil, nil)
	require.False(t, shouldForwardOpenAIResponsesViaRawChatCompletions(h.account),
		"force_responses keeps /v1/messages on the Responses path; the Chat fallbacks are unreachable for Prism")
	messages := make([]string, 0, 15)
	for i := 0; i < 7; i++ {
		messages = append(messages,
			`{"role":"user","content":"question `+strings.Repeat("x", i)+`"}`,
			`{"role":"assistant","content":[{"type":"text","text":"answer `+strings.Repeat("x", i)+`"}]}`)
	}
	messages = append(messages, `{"role":"user","content":"final question"}`)
	body := `{"model":"gpt-6.1-sol","max_tokens":64,"stream":false,"system":"You are Claude Code.",
		"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}],
		"messages":[` + strings.Join(messages, ",") + `]}`
	recorder, _, err := h.forward("messages", "/v1/messages", []byte(body), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	upstreamBody := h.upstreamBody(t)
	require.Equal(t, "/v1/responses", strings.TrimPrefix(h.upstream.lastReq.URL.Path, "/accounts/32"))
	require.Equal(t, "final question", gjson.GetBytes(upstreamBody, `input.#(role=="user")#|@reverse|0.content.0.text`).String())
	require.Equal(t, "question ", gjson.GetBytes(upstreamBody, `input.#(role=="user")#|0.content.0.text`).String(),
		"the sliding replay window built for previous_response_id must not drop Prism's history")
	require.False(t, gjson.GetBytes(upstreamBody, "previous_response_id").Exists())
	require.Equal(t, "Bash", gjson.GetBytes(upstreamBody, "tools.0.name").String())
	require.Equal(t, []int32{h.runtime.Load()}, h.upstream.runtimeCallsAtUpstream)
	require.NotZero(t, h.upstream.runtimeCallsAtUpstream[0], "the Prism session must be ensured before the upstream call")

	// A second turn must not pick up a stored response id as continuation.
	h.upstream.bodies, h.upstream.requests = nil, nil
	recorder, _, err = h.forward("messages", "/v1/messages", []byte(body), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.False(t, gjson.GetBytes(h.upstreamBody(t), "previous_response_id").Exists())
}

func TestPrismGatewayFailsOverUnservableRequestsBeforeTouchingPrism(t *testing.T) {
	image := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`
	for _, test := range []struct {
		name, family, path, body string
		reason                   GatewayFailureReason
	}{
		{name: "responses_image", family: "responses", path: "/v1/responses", reason: PrismNonTextInputReason,
			body: `{"model":"gpt-6.1-sol","input":[{"role":"user","content":[{"type":"input_text","text":"see"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`},
		{name: "responses_previous_response_id", family: "responses", path: "/v1/responses", reason: PrismPreviousResponseUnsupportedReason,
			body: `{"model":"gpt-6.1-sol","input":"next","previous_response_id":"resp_1"}`},
		{name: "responses_compact", family: "responses", path: "/v1/responses/compact", reason: PrismCompactUnsupportedReason,
			body: `{"model":"gpt-6.1-sol","input":"summarize"}`},
		{name: "chat_image", family: "chat", path: "/v1/chat/completions", reason: PrismNonTextInputReason,
			body: `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]}]}`},
		{name: "messages_image", family: "messages", path: "/v1/messages", reason: PrismNonTextInputReason,
			body: `{"model":"gpt-6.1-sol","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"see"},` + image + `]}]}`},
		{name: "messages_tool_result_image", family: "messages", path: "/v1/messages", reason: PrismNonTextInputReason,
			body: `{"model":"gpt-6.1-sol","max_tokens":16,"messages":[{"role":"user","content":"read it"},
				{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` + image + `]}]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPrismGatewayHarness(t, nil, nil)
			recorder, c, err := h.forward(test.family, test.path, []byte(test.body), nil)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, test.reason, failoverErr.Reason)
			require.True(t, failoverErr.IsPrismRequestUnsupported())
			require.True(t, failoverErr.ShouldRetryNextAccount())
			require.False(t, failoverErr.ShouldReportAccountScheduleFailure())
			require.False(t, failoverErr.RetryableOnSameAccount)
			require.Empty(t, recorder.Body.String(), "the handler, not the forwarder, answers the client")
			require.False(t, c.Writer.Written())
			require.Empty(t, h.upstream.requests, "the sidecar must never see the request")
			require.Zero(t, h.runtime.Load(), "no sidecar session work for a request Prism cannot serve")
			account, getErr := h.repo.GetByID(context.Background(), h.account.ID)
			require.NoError(t, getErr)
			require.Nil(t, account.TempUnschedulableUntil, "the account must not be cooled down")
			require.True(t, account.Schedulable)
		})
	}
}

func TestPrismGatewayRejectsMalformedRequestWithoutFailover(t *testing.T) {
	for _, family := range []string{"responses", "chat"} {
		t.Run(family, func(t *testing.T) {
			h := newPrismGatewayHarness(t, nil, nil)
			path := "/v1/responses"
			if family == "chat" {
				path = "/v1/chat/completions"
			}
			recorder, _, err := h.forward(family, path, []byte(`{"model":`), nil)
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr))
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Contains(t, recorder.Body.String(), "invalid_request_error")
			require.Empty(t, h.upstream.requests)
		})
	}
}

// A Prism account whose browser session is down is an unavailable account like
// any other: the handler gets a failover error with account scope (so it picks
// the next account and the scheduler hears about the failure), and nothing has
// reached the sidecar's model endpoint or the client.
func TestPrismGatewaySessionFailureFailsOverBeforeAnythingIsWritten(t *testing.T) {
	requests := map[string][2]string{
		"responses": {"/v1/responses", `{"model":"gpt-6.1-sol","input":"hello","stream":false}`},
		"chat":      {"/v1/chat/completions", `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}],"stream":false}`},
		"messages":  {"/v1/messages", `{"model":"gpt-6.1-sol","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`},
	}
	for _, family := range []string{"responses", "chat", "messages"} {
		for _, failure := range []string{"not_ready", "sidecar_503", "sidecar_unreachable", "not_configured"} {
			t.Run(family+"_"+failure, func(t *testing.T) {
				h := newPrismGatewayHarness(t, nil, nil)
				switch failure {
				case "not_ready":
					h.runtimeMode.Store(prismRuntimeNotReady)
				case "sidecar_503":
					h.runtimeMode.Store(prismRuntimeFailing)
				case "sidecar_unreachable":
					h.runtimeServer.Close()
				case "not_configured":
					h.svc.prismAccounts = nil
				}
				recorder, c, err := h.forward(family, requests[family][0], []byte(requests[family][1]), nil)

				var failoverErr *UpstreamFailoverError
				require.ErrorAs(t, err, &failoverErr)
				require.Equal(t, PrismSessionUnavailableReason, failoverErr.Reason)
				require.Equal(t, GatewayFailureScopeAccount, failoverErr.Scope)
				require.Equal(t, GatewayFailureStageInference, failoverErr.Stage)
				require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode)
				require.True(t, failoverErr.ShouldRetryNextAccount(), "the handler moves on to the next account")
				require.True(t, failoverErr.ShouldReportAccountScheduleFailure(), "a down session is an account fault: health reporting applies")
				require.False(t, failoverErr.IsPrismRequestUnsupported())
				require.False(t, failoverErr.IsCredentialFailure())
				require.False(t, failoverErr.RetryableOnSameAccount, "the account is already cooling down; retrying it only delays failover")
				require.False(t, failoverErr.RequestScopedTransient)
				require.NotEmpty(t, gjson.GetBytes(failoverErr.ResponseBody, "error.code").String(), "ops sees which Prism condition it was")
				require.NotContains(t, string(failoverErr.ResponseBody), "127.0.0.1", "no internal addresses in the ops body")
				require.Empty(t, recorder.Body.String(), "nothing may be written before the next account is tried")
				require.False(t, c.Writer.Written())
				require.False(t, IsResponseCommitted(c))
				require.Empty(t, h.upstream.requests, "the sidecar's model endpoint must not be called")

				account, getErr := h.repo.GetByID(context.Background(), h.account.ID)
				require.NoError(t, getErr)
				if failure == "not_configured" {
					require.Nil(t, account.TempUnschedulableUntil, "no session check ran, so there is nothing to cool down")
				} else {
					require.NotNil(t, account.TempUnschedulableUntil, "a failed session check cools the account down")
					require.Contains(t, account.TempUnschedulableReason, "prism:")
				}
			})
		}
	}
}

func TestPrismGatewayCanceledRequestDoesNotCoolDownThePrismAccount(t *testing.T) {
	h := newPrismGatewayHarness(t, nil, nil)
	h.runtimeMode.Store(prismRuntimeNotReady)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := h.svc.ensurePrismSession(ctx, h.account)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "a vanished client says nothing about the session")
	account, getErr := h.repo.GetByID(context.Background(), h.account.ID)
	require.NoError(t, getErr)
	require.Nil(t, account.TempUnschedulableUntil)
}

// The sidecar answering a model request with HTTP 503 account_not_ready (or any
// other 5xx) takes the ordinary API-key failover path on every entry point: a
// failover error with the upstream status, health reporting on, no same-account
// retry for a non-pool account, and nothing written to the client.
func TestPrismGatewaySidecar5xxTakesTheRegularAPIKeyFailoverPath(t *testing.T) {
	requests := map[string][2]string{
		"responses": {"/v1/responses", `{"model":"gpt-6.1-sol","input":"hello","stream":false}`},
		"chat":      {"/v1/chat/completions", `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}],"stream":false}`},
		"messages":  {"/v1/messages", `{"model":"gpt-6.1-sol","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`},
	}
	for _, family := range []string{"responses", "chat", "messages"} {
		for _, test := range []struct {
			name, body string
			status     int
		}{
			{"account_not_ready", `{"error":{"code":"account_not_ready","message":"The Prism account is not ready"}}`, http.StatusServiceUnavailable},
			{"heartbeat_stale", `{"error":{"code":"heartbeat_stale"}}`, http.StatusServiceUnavailable},
			{"internal_error", `{"error":{"code":"browser_operation_failed"}}`, http.StatusInternalServerError},
			{"bad_gateway", `{"error":{"code":"browser_operation_failed"}}`, http.StatusBadGateway},
			{"timeout", `{"error":{"code":"request_timeout"}}`, http.StatusGatewayTimeout},
		} {
			t.Run(family+"_"+test.name, func(t *testing.T) {
				h := newPrismGatewayHarness(t, nil, nil)
				h.upstreamAnswer = &http.Response{StatusCode: test.status, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(test.body))}
				recorder, c, err := h.forward(family, requests[family][0], []byte(requests[family][1]), nil)

				var failoverErr *UpstreamFailoverError
				require.ErrorAs(t, err, &failoverErr)
				require.Equal(t, test.status, failoverErr.StatusCode)
				require.True(t, failoverErr.ShouldRetryNextAccount())
				require.True(t, failoverErr.ShouldReportAccountScheduleFailure())
				require.False(t, failoverErr.IsPrismRequestUnsupported())
				require.NotEqual(t, PrismSessionUnavailableReason, failoverErr.Reason)
				require.False(t, failoverErr.RetryableOnSameAccount, "Prism accounts are not pool mode")
				require.Equal(t, test.body, string(failoverErr.ResponseBody))
				require.Empty(t, recorder.Body.String())
				require.False(t, c.Writer.Written())
				require.Len(t, h.upstream.requests, 1, "no same-account retry")
				require.NotZero(t, h.runtime.Load(), "the session was ensured before the call")
			})
		}
	}
}

// A 400 from the sidecar (unknown model) is not an account fault: the client
// sees it instead of the request being replayed on other accounts.
func TestPrismGatewaySidecarClientErrorIsNotFailedOver(t *testing.T) {
	h := newPrismGatewayHarness(t, nil, nil)
	h.upstreamAnswer = &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"error":{"code":"model_not_available","message":"model is not in the catalog"}}`))}
	recorder, _, err := h.forward("responses", "/v1/responses", []byte(`{"model":"gpt-6.1-sol","input":"hello","stream":false}`), nil)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

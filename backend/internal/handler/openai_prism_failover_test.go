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

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const prismServedText = "served by the other account"

func prismFailoverPrismAccount(id int64) service.Account {
	return service.Account{
		ID: id, Name: fmt.Sprintf("prism-%d", id), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{3131}, Priority: 0, Concurrency: 1,
		Credentials: map[string]any{"api_key": strings.Repeat("k", 64), "base_url": "http://127.0.0.1:8319/accounts/32/v1",
			"openai_capabilities": []string{"chat_completions"}},
		Extra: map[string]any{"provider_preset": service.PrismProviderPreset, service.PrismSourceAccountKey: int64(32),
			"openai_responses_mode": "force_responses", "openai_passthrough": false,
			"openai_apikey_responses_websockets_v2_mode": "http_bridge"},
	}
}

func prismFailoverRegularAccount(id int64) service.Account {
	return service.Account{
		ID: id, Name: "regular", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{3131}, Priority: 5, Concurrency: 1,
		Credentials: map[string]any{"api_key": "regular-key", "base_url": "https://api.example.test/v1"},
	}
}

func prismFailoverUpstreamAnswer(int) *http.Response {
	wire := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + prismServedText + "\",\"output_index\":0,\"content_index\":0}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_regular\",\"status\":\"completed\",\"model\":\"gpt-6-astra\"," +
		"\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"" + prismServedText + "\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(wire))}
}

// newPrismFailoverHandler schedules the Prism accounts ahead of the regular one.
// The Prism service is deliberately absent: a request Prism cannot serve must be
// declined before any session work, so reaching it would fail the test.
func newPrismFailoverHandler(t *testing.T, upstream service.HTTPUpstream, accounts ...service.Account) *OpenAIGatewayHandler {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	concurrency := service.NewConcurrencyService(nil)
	gateway := service.NewOpenAIGatewayService(
		excelBPSFailoverAccountRepo{openAIImagesFailoverAccountRepo{accounts: accounts}},
		nil, nil, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, nil, nil, upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	handler := NewOpenAIGatewayHandler(gateway, concurrency, billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	handler.maxAccountSwitches = 1
	return handler
}

func newPrismFailoverContext(path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	groupID := int64(3131)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 99, GroupID: &groupID, User: &service.User{ID: 100},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowMessagesDispatch: true},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	return c, rec
}

type prismFailoverRequest struct {
	name, family, path, body string
	code                     string
}

func prismFailoverRequests() []prismFailoverRequest {
	return []prismFailoverRequest{
		{name: "responses_audio", family: "responses", path: "/v1/responses", code: "image_input_not_supported",
			body: `{"model":"gpt-6-astra","stream":false,"input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`},
		{name: "responses_previous_response_id", family: "responses", path: "/v1/responses", code: "previous_response_not_supported",
			body: `{"model":"gpt-6-astra","stream":false,"input":"next","previous_response_id":"resp_123"}`},
		{name: "responses_compact", family: "responses", path: "/v1/responses/compact", code: "compact_not_supported",
			body: `{"model":"gpt-6-astra","input":"summarize the conversation"}`},
		{name: "chat_audio", family: "chat", path: "/v1/chat/completions", code: "image_input_not_supported",
			body: `{"model":"gpt-6-astra","stream":false,"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`},
		{name: "messages_audio", family: "messages", path: "/v1/messages", code: "image_input_not_supported",
			body: `{"model":"gpt-6-astra","max_tokens":16,"stream":false,"messages":[{"role":"user","content":[{"type":"audio","source":{"type":"base64","media_type":"audio/wav","data":"AAAA"}}]}]}`},
	}
}

// ownPreviousResponse registers resp_123 as the caller's earlier response, which
// the Responses handler requires before it accepts previous_response_id.
func ownPreviousResponse(t *testing.T, handler *OpenAIGatewayHandler) {
	t.Helper()
	require.NoError(t, handler.gatewayService.BindOpenAIHTTPResponseOwner(context.Background(), 3131, "resp_123", 100, 99))
}

func serveForFamily(handler *OpenAIGatewayHandler, family string, c *gin.Context) {
	switch family {
	case "chat":
		handler.ChatCompletions(c)
	case "messages":
		handler.Messages(c)
	default:
		handler.Responses(c)
	}
}

// Requests Prism cannot serve move on to the next account without spending
// switch budget (maxAccountSwitches is 1 here, against three Prism accounts)
// and without any scheduler penalty for the skipped accounts.
func TestOpenAIPrismUnservableRequestFailsOverToAnotherAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range prismFailoverRequests() {
		t.Run(test.name, func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: prismFailoverUpstreamAnswer}
			handler := newPrismFailoverHandler(t, upstream,
				prismFailoverPrismAccount(11), prismFailoverPrismAccount(12), prismFailoverPrismAccount(13), prismFailoverRegularAccount(2))
			ownPreviousResponse(t, handler)
			c, rec := newPrismFailoverContext(test.path, test.body)

			serveForFamily(handler, test.family, c)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), prismServedText)
			require.Equal(t, []int64{2}, upstream.calls(), "only the regular account may receive the request")
			_, recordedUpstreamError := c.Get(service.OpsUpstreamErrorsKey)
			require.False(t, recordedUpstreamError, "a declined request is not an upstream failure")
		})
	}
}

// With nothing else to try the client gets a clear 400 in its own error format.
func TestOpenAIPrismUnservableRequestReturnsClearBadRequestWhenNoAccountRemains(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range prismFailoverRequests() {
		t.Run(test.name, func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response { t.Fatal("unexpected upstream call"); return nil }}
			handler := newPrismFailoverHandler(t, upstream, prismFailoverPrismAccount(11), prismFailoverPrismAccount(12))
			ownPreviousResponse(t, handler)
			c, rec := newPrismFailoverContext(test.path, test.body)

			serveForFamily(handler, test.family, c)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Empty(t, upstream.calls())
			require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.NotContains(t, rec.Body.String(), "single user text message")
			require.Contains(t, rec.Body.String(), "Prism")
			if test.family != "messages" {
				require.Equal(t, test.code, gjson.Get(rec.Body.String(), "error.code").String())
			} else {
				require.Equal(t, "error", gjson.Get(rec.Body.String(), "type").String())
			}
		})
	}
}

// A fault from another account stays the answer when Prism then declines the
// request: the client should learn what actually went wrong.
func TestOpenAIPrismDeclineDoesNotMaskAnotherAccountsFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"message":"overloaded"}}`))}
	}}
	regular := prismFailoverRegularAccount(2)
	regular.Priority = 0
	prism := prismFailoverPrismAccount(11)
	prism.Priority = 1
	handler := newPrismFailoverHandler(t, upstream, regular, prism)
	handler.maxAccountSwitches = 3
	request := prismFailoverRequests()[0]
	c, rec := newPrismFailoverContext(request.path, request.body)

	handler.Responses(c)

	require.Equal(t, []int64{2}, upstream.calls())
	require.NotEqual(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "Prism")
}

// Text traffic is never declined: it proceeds to the sidecar session check,
// which is absent here and therefore fails before anything is sent upstream.
func TestOpenAIPrismTextRequestIsNotDeclined(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response { t.Fatal("unexpected upstream call"); return nil }}
	handler := newPrismFailoverHandler(t, upstream, prismFailoverPrismAccount(11))
	c, rec := newPrismFailoverContext("/v1/responses",
		`{"model":"gpt-6-astra","stream":false,"instructions":"be brief","input":[{"role":"user","content":"hello"}],"tools":[{"type":"web_search"}]}`)

	handler.Responses(c)

	require.NotEqual(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "image_input_not_supported")
	require.Empty(t, upstream.calls())
}

func prismSessionFailureRequests() []prismFailoverRequest {
	return []prismFailoverRequest{
		{name: "responses", family: "responses", path: "/v1/responses",
			body: `{"model":"gpt-6-astra","stream":false,"instructions":"be brief","input":[{"role":"user","content":"hello"}]}`},
		{name: "chat", family: "chat", path: "/v1/chat/completions",
			body: `{"model":"gpt-6-astra","stream":false,"messages":[{"role":"user","content":"hello"}]}`},
		{name: "messages", family: "messages", path: "/v1/messages",
			body: `{"model":"gpt-6-astra","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hello"}]}`},
	}
}

// A Prism account whose browser session is down (here: the Prism service is not
// configured, so every session check fails) is an unavailable account: the
// handler fails over to the next one instead of answering with a bare 502.
func TestOpenAIPrismSessionFailureFailsOverToAnotherAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range prismSessionFailureRequests() {
		t.Run(test.name, func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: prismFailoverUpstreamAnswer}
			handler := newPrismFailoverHandler(t, upstream, prismFailoverPrismAccount(11), prismFailoverRegularAccount(2))
			c, rec := newPrismFailoverContext(test.path, test.body)

			serveForFamily(handler, test.family, c)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), prismServedText)
			require.Equal(t, []int64{2}, upstream.calls(), "only the regular account may receive the request")
		})
	}
}

// With no other account the client gets the standard unavailable-upstream answer.
func TestOpenAIPrismSessionFailureWithoutAnotherAccountReturnsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range prismSessionFailureRequests() {
		t.Run(test.name, func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response { t.Fatal("unexpected upstream call"); return nil }}
			handler := newPrismFailoverHandler(t, upstream, prismFailoverPrismAccount(11))
			c, rec := newPrismFailoverContext(test.path, test.body)

			serveForFamily(handler, test.family, c)

			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "temporarily unavailable")
			require.NotContains(t, rec.Body.String(), "PRISM_")
			require.Empty(t, upstream.calls())
		})
	}
}

func TestSkipPrismUnsupportedAccountKeepsAnEarlierFailure(t *testing.T) {
	declined := &service.UpstreamFailoverError{Reason: service.PrismNonTextInputReason}
	earlier := &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway}
	failed := map[int64]struct{}{}
	last := earlier
	require.True(t, skipPrismUnsupportedAccount(declined, 7, failed, &last))
	require.Contains(t, failed, int64(7))
	require.Same(t, earlier, last)
	last = nil
	require.True(t, skipPrismUnsupportedAccount(declined, 8, failed, &last))
	require.Same(t, declined, last)
	require.False(t, skipPrismUnsupportedAccount(earlier, 9, failed, &last))
	require.NotContains(t, failed, int64(9))
}

//go:build unit

package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type prismWSTokens struct{}

func (prismWSTokens) GetAccessToken(context.Context, *service.Account) (string, error) {
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/profile": map[string]any{"email": "prism-owner@example.test"}})
	return "test." + base64.RawURLEncoding.EncodeToString(claims) + ".signature", nil
}

type prismWSFixture struct {
	t        *testing.T
	url      string
	upstream *excelBPSFailoverUpstream
	handler  *OpenAIGatewayHandler
}

// newPrismWSFixture serves the real WebSocket handler. prismReady attaches a
// Prism service whose fake sidecar reports a ready session; without it every
// Prism account fails its session check (the service is not configured).
func newPrismWSFixture(t *testing.T, prismReady bool, accounts ...service.Account) *prismWSFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP, cfg.Security.URLAllowlist.AllowPrivateHosts = true, true
	ws := &cfg.Gateway.OpenAIWS
	ws.Enabled, ws.APIKeyEnabled, ws.ResponsesWebsocketsV2, ws.ModeRouterV2Enabled = true, true, true, true
	ws.IngressModeDefault = service.OpenAIWSIngressModeOff

	source := service.Account{ID: 32, Name: "source", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: false, Credentials: map[string]any{"access_token": "x", "email": "prism-owner@example.test"}}
	repo := excelBPSFailoverAccountRepo{openAIImagesFailoverAccountRepo{accounts: append([]service.Account{source}, accounts...)}}
	upstream := &excelBPSFailoverUpstream{answer: prismFailoverUpstreamAnswer}
	deps := service.OpenAIGatewayDependencies{Accounts: repo, Config: cfg, Upstream: upstream}
	if prismReady {
		runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"phase":"ready","ready":true,"models":["gpt-6-astra"]}`))
		}))
		t.Cleanup(runtime.Close)
		for i := range repo.accounts {
			if repo.accounts[i].IsManagedPrismAccount() {
				repo.accounts[i].Credentials["base_url"] = runtime.URL + "/accounts/32/v1"
			}
		}
		prism := service.NewPrismAccountService(repo, nil, prismWSTokens{}, service.PrismRuntimeConfig{URL: runtime.URL, ManagementKey: "management-test-secret"})
		t.Cleanup(prism.Stop)
		deps.PrismAccounts = prism
	}
	concurrency := service.NewConcurrencyService(nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	handler := NewOpenAIGatewayHandler(service.ProvideOpenAIGatewayService(deps), concurrency, billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	handler.maxAccountSwitches = 0

	groupID := int64(3131)
	apiKey := &service.APIKey{ID: 99, GroupID: &groupID, User: &service.User{ID: 100, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI}}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
		c.Next()
	})
	router.GET("/openai/v1/responses", handler.ResponsesWebSocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return &prismWSFixture{t: t, url: "ws" + strings.TrimPrefix(server.URL, "http") + "/openai/v1/responses", upstream: upstream, handler: handler}
}

// exchange sends one frame on a fresh connection and returns the text of the
// frames read until the response completes or the connection closes, plus the
// close error (nil when the turn completed).
func (f *prismWSFixture) exchange(frame string) ([]string, error) {
	f.t.Helper()
	dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(dialCtx, f.url, nil)
	require.NoError(f.t, err)
	defer func() { _ = client.CloseNow() }()
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelWrite()
	require.NoError(f.t, client.Write(writeCtx, coderws.MessageText, []byte(frame)))
	var events []string
	for {
		readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
		_, event, readErr := client.Read(readCtx)
		cancelRead()
		if readErr != nil {
			return events, readErr
		}
		events = append(events, string(event))
		if gjson.GetBytes(event, "type").String() == "response.completed" {
			return events, nil
		}
	}
}

func prismWSBridgedRegular(id int64) service.Account {
	account := prismFailoverRegularAccount(id)
	account.Extra = map[string]any{"openai_apikey_responses_websockets_v2_mode": service.OpenAIWSIngressModeHTTPBridge}
	return account
}

const (
	prismWSTextFrame  = `{"type":"response.create","model":"gpt-6-astra","instructions":"be brief","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`
	prismWSImageFrame = `{"type":"response.create","model":"gpt-6-astra","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`
	prismWSAudioFrame = `{"type":"response.create","model":"gpt-6-astra","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`
)

// A Prism-only group serves Codex WebSocket clients: the turn is bridged to the
// sidecar's HTTP /responses endpoint instead of the client being closed with
// "no available account".
func TestOpenAIPrismWebSocketClientIsServedThroughTheHTTPBridge(t *testing.T) {
	f := newPrismWSFixture(t, true, prismFailoverPrismAccount(11))

	events, err := f.exchange(prismWSTextFrame)

	require.NoError(t, err)
	require.Contains(t, events[len(events)-1], prismServedText)
	require.Equal(t, []int64{11}, f.upstream.calls())
	require.Contains(t, f.upstream.urls[0], "/accounts/32/v1/responses", "the turn goes to the sidecar over HTTP, not WebSocket")
	require.Equal(t, "be brief", gjson.GetBytes(f.upstream.bodies[0], "instructions").String())
	require.True(t, gjson.GetBytes(f.upstream.bodies[0], "stream").Bool())
	require.Equal(t, "none", f.upstream.headers[0].Get("X-Prism-Failover"))
}

func TestOpenAIPrismWebSocketSignalsAnAvailableFailover(t *testing.T) {
	f := newPrismWSFixture(t, true, prismFailoverPrismAccount(11), prismWSBridgedRegular(2))
	_, err := f.exchange(prismWSTextFrame)
	require.NoError(t, err)
	require.Equal(t, "available", f.upstream.headers[0].Get("X-Prism-Failover"))
}

func TestOpenAIPrismHTTPOverwritesTheClientFailoverHeader(t *testing.T) {
	for _, available := range []bool{false, true} {
		accounts := []service.Account{prismFailoverPrismAccount(11)}
		if available {
			accounts = append(accounts, prismFailoverRegularAccount(2))
		}
		fixture := newPrismWSFixture(t, true, accounts...)
		ctx, recorder := newPrismFailoverContext("/v1/responses", `{"model":"gpt-6-astra","input":"hello"}`)
		ctx.Request.Header.Set("X-Prism-Failover", "client-controlled")
		fixture.handler.Responses(ctx)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, []int64{11}, fixture.upstream.calls())
		expected := "none"
		if available {
			expected = "available"
		}
		require.Equal(t, expected, fixture.upstream.headers[0].Get("X-Prism-Failover"))
	}
}

func TestOpenAIPrismWebSocketPreservesImageAttachments(t *testing.T) {
	f := newPrismWSFixture(t, true, prismFailoverPrismAccount(11))
	events, err := f.exchange(prismWSImageFrame)
	require.NoError(t, err)
	require.Contains(t, events[len(events)-1], prismServedText)
	require.Equal(t, []int64{11}, f.upstream.calls())
	require.Equal(t, "input_image", gjson.GetBytes(f.upstream.bodies[0], "input.0.content.1.type").String())
	require.Equal(t, "data:image/png;base64,AAAA", gjson.GetBytes(f.upstream.bodies[0], "input.0.content.1.image_url").String())
}

// Declining a frame costs no switch budget (maxAccountSwitches is 0 here) and
// no penalty; the next account serves it.
func TestOpenAIPrismWebSocketDeclinedFrameMovesToAnotherAccount(t *testing.T) {
	f := newPrismWSFixture(t, true, prismFailoverPrismAccount(11), prismWSBridgedRegular(2))

	events, err := f.exchange(prismWSAudioFrame)

	require.NoError(t, err)
	require.Contains(t, events[len(events)-1], prismServedText)
	require.Equal(t, []int64{2}, f.upstream.calls(), "only the regular account may receive the audio")
}

// A down session is an account fault and takes the normal failover path, which
// does spend switch budget.
func TestOpenAIPrismWebSocketSessionFailureMovesToAnotherAccount(t *testing.T) {
	f := newPrismWSFixture(t, false, prismFailoverPrismAccount(11), prismWSBridgedRegular(2))
	f.handler.maxAccountSwitches = 1

	events, err := f.exchange(prismWSTextFrame)

	require.NoError(t, err)
	require.Contains(t, events[len(events)-1], prismServedText)
	require.Equal(t, []int64{2}, f.upstream.calls())
}

// With nothing else to try, the client is told why instead of seeing a generic
// upstream failure.
func TestOpenAIPrismWebSocketDeclinedFrameClosesWithAClearReasonWhenNoAccountRemains(t *testing.T) {
	f := newPrismWSFixture(t, true, prismFailoverPrismAccount(11))

	events, err := f.exchange(prismWSAudioFrame)

	require.Empty(t, events)
	var closeErr coderws.CloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.Code)
	require.Contains(t, closeErr.Reason, "Prism accounts do not support audio input")
	require.Empty(t, f.upstream.calls())
}

func TestOpenAIPrismWebSocketSessionFailureWithoutAnotherAccountClosesAsUnavailable(t *testing.T) {
	f := newPrismWSFixture(t, false, prismFailoverPrismAccount(11))
	f.handler.maxAccountSwitches = 1

	events, err := f.exchange(prismWSTextFrame)

	require.Empty(t, events)
	var closeErr coderws.CloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusTryAgainLater, closeErr.Code)
	require.Contains(t, closeErr.Reason, "temporarily unavailable")
	require.Empty(t, f.upstream.calls())
}

// Accounts that are not bridged yet (still "off") keep being skipped for WS.
func TestOpenAIPrismWebSocketAccountWithoutBridgeModeIsNotScheduled(t *testing.T) {
	legacy := prismFailoverPrismAccount(11)
	legacy.Extra["openai_apikey_responses_websockets_v2_mode"] = "off" // created before WS clients were bridged
	f := newPrismWSFixture(t, true, legacy, prismWSBridgedRegular(2))

	events, err := f.exchange(prismWSTextFrame)

	require.NoError(t, err)
	require.Contains(t, events[len(events)-1], prismServedText)
	require.Equal(t, []int64{2}, f.upstream.calls())
}

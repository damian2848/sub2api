//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// prismWSBridge runs ProxyResponsesWebSocketFromClient for a Prism account the
// way the WS handler does: one upgraded connection, frames in, frames out, the
// proxy's final error on errCh.
type prismWSBridge struct {
	t      *testing.T
	h      *prismGatewayHarness
	client *coderws.Conn
	errCh  chan error
}

func newPrismWSBridge(t *testing.T, h *prismGatewayHarness, routerV2 bool, responses ...*http.Response) *prismWSBridge {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ws := &h.svc.cfg.Gateway.OpenAIWS
	ws.Enabled, ws.APIKeyEnabled, ws.ResponsesWebsocketsV2 = true, true, true
	ws.ModeRouterV2Enabled = routerV2
	ws.IngressModeDefault = OpenAIWSIngressModeOff
	h.svc.cache = &stubGatewayCache{}
	h.svc.toolCorrector = NewCodexToolCorrector()
	h.upstream.responses = responses

	bridge := &prismWSBridge{t: t, h: h, errCh: make(chan error, 1)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			bridge.errCh <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		readCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		_, first, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			bridge.errCh <- err
			return
		}
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		req := r.Clone(r.Context())
		req.Header = req.Header.Clone()
		req.Header.Set("User-Agent", "codex_cli_rs/0.135.0")
		ginCtx.Request = req
		bridge.errCh <- h.svc.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, conn, h.account, h.account.GetCredential("api_key"), first, nil)
	}))
	t.Cleanup(server.Close)
	dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.CloseNow() })
	bridge.client = client
	return bridge
}

func (b *prismWSBridge) send(frame string) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(b.t, b.client.Write(ctx, coderws.MessageText, []byte(frame)))
}

// completed reads frames until the turn's terminal event and returns them.
func (b *prismWSBridge) completed() [][]byte {
	b.t.Helper()
	var events [][]byte
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, event, err := b.client.Read(ctx)
		cancel()
		require.NoError(b.t, err)
		events = append(events, event)
		if gjson.GetBytes(event, "type").String() == "response.completed" {
			return events
		}
	}
}

func (b *prismWSBridge) finish() error {
	b.t.Helper()
	select {
	case err := <-b.errCh:
		return err
	case <-time.After(5 * time.Second):
		b.t.Fatal("timed out waiting for the websocket bridge to finish")
		return nil
	}
}

func prismWSTurnResponse(id string) *http.Response {
	message := `{"type":"message","id":"msg_` + id + `","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer-` + id + `"}]}`
	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"` + id + `","model":"gpt-6.1-sol"}}`, "",
		`data: {"type":"response.output_text.delta","response":{"id":"` + id + `"},"delta":"answer-` + id + `"}`, "",
		`data: {"type":"response.output_item.done","item":` + message + `}`, "",
		`data: {"type":"response.completed","response":{"id":"` + id + `","model":"gpt-6.1-sol","output":[` + message + `],"usage":{"input_tokens":3,"output_tokens":1}}}`, "",
	}, "\n")
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(body))}
}

// Codex repeats instructions and tools on every frame and sends only the new
// input items together with previous_response_id.
func prismWSFrame(text, previousResponseID string) string {
	previous := ""
	if previousResponseID != "" {
		previous = `"previous_response_id":"` + previousResponseID + `",`
	}
	return `{"type":"response.create","model":"gpt-6.1-sol","instructions":"You are Codex.","store":false,"service_tier":"priority",` + previous +
		`"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}]}`
}

// Serving a WebSocket client from Prism means bridging every turn to the
// sidecar's HTTP /responses endpoint: Prism rejects previous_response_id, so the
// bridge has to send the whole conversation, including what the model said.
func TestPrismWebSocketBridgeServesEveryTurnOverHTTPWithFullHistory(t *testing.T) {
	for _, routerV2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("mode_router_v2=%t", routerV2), func(t *testing.T) {
			h := newPrismGatewayHarness(t, nil, nil)
			bridge := newPrismWSBridge(t, h, routerV2, prismWSTurnResponse("r1"), prismWSTurnResponse("r2"), prismWSTurnResponse("r3"))

			bridge.send(prismWSFrame("first question", ""))
			first := bridge.completed()
			require.Equal(t, "response.created", gjson.GetBytes(first[0], "type").String(), "SSE events arrive as WS frames")
			require.Equal(t, "response.output_text.delta", gjson.GetBytes(first[1], "type").String())
			bridge.send(prismWSFrame("second question", "r1"))
			second := bridge.completed()
			require.Equal(t, "r2", gjson.GetBytes(second[len(second)-1], "response.id").String())
			bridge.send(prismWSFrame("third question", "r2"))
			bridge.completed()
			require.NoError(t, bridge.client.Close(coderws.StatusNormalClosure, "done"))
			require.NoError(t, bridge.finish())

			require.Len(t, h.upstream.bodies, 3)
			for i, request := range h.upstream.requests {
				require.Equal(t, "/accounts/32/v1/responses", request.URL.Path, "turn %d goes to the sidecar over HTTP", i+1)
				require.Equal(t, "Bearer "+strings.Repeat("k", 64), request.Header.Get("Authorization"),
					"turn %d authenticates with the credential the handler obtained for the account", i+1)
				body := h.upstream.bodies[i]
				require.Equal(t, "true", gjson.GetBytes(body, "stream").Raw)
				for _, field := range []string{"previous_response_id", "type", "generate", "service_tier"} {
					require.False(t, gjson.GetBytes(body, field).Exists(), "turn %d must not carry %s", i+1, field)
				}
				require.Equal(t, "You are Codex.", gjson.GetBytes(body, "instructions").String())
				require.Equal(t, "shell", gjson.GetBytes(body, "tools.0.name").String())
			}
			text := func(body []byte, index int) string {
				item := gjson.GetBytes(body, fmt.Sprintf("input.%d", index))
				return item.Get("role").String() + ":" + item.Get("content.0.text").String()
			}
			require.Equal(t, int64(1), gjson.GetBytes(h.upstream.bodies[0], "input.#").Int())
			require.Equal(t, int64(3), gjson.GetBytes(h.upstream.bodies[1], "input.#").Int())
			require.Equal(t, "user:first question", text(h.upstream.bodies[1], 0))
			require.Equal(t, "assistant:answer-r1", text(h.upstream.bodies[1], 1), "what the model said must be replayed")
			require.Equal(t, "user:second question", text(h.upstream.bodies[1], 2))
			require.Equal(t, int64(5), gjson.GetBytes(h.upstream.bodies[2], "input.#").Int())
			require.Equal(t, "assistant:answer-r1", text(h.upstream.bodies[2], 1))
			require.Equal(t, "assistant:answer-r2", text(h.upstream.bodies[2], 3))
			require.Equal(t, "user:third question", text(h.upstream.bodies[2], 4))

			require.Len(t, h.upstream.runtimeCallsAtUpstream, 3)
			for i := 1; i < 3; i++ {
				require.Greater(t, h.upstream.runtimeCallsAtUpstream[i], h.upstream.runtimeCallsAtUpstream[i-1],
					"the session is checked again before every turn, not only when the socket opens")
			}
			require.NotZero(t, h.upstream.runtimeCallsAtUpstream[0])
		})
	}
}

// What the bridge cannot serve must fail over, never reach the sidecar, and
// never write to the client: on the first turn the handler picks another
// account, on a later turn the current turn is retried there with the history.
func TestPrismWebSocketBridgeFailsOverTurnsPrismCannotServe(t *testing.T) {
	image := `{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}`
	for _, test := range []struct {
		name     string
		firstTry string // frame of the first turn
		retry    bool   // a normal first turn precedes the failing one
		second   string
		reason   GatewayFailureReason
		prepare  func(*prismGatewayHarness)
	}{
		{name: "first_turn_image", reason: PrismNonTextInputReason,
			firstTry: `{"type":"response.create","model":"gpt-6.1-sol","input":[` + image + `]}`},
		{name: "first_turn_resumed_session_without_history", reason: PrismPreviousResponseUnsupportedReason,
			firstTry: prismWSFrame("continue", "resp_from_elsewhere")},
		{name: "later_turn_image", retry: true, reason: PrismNonTextInputReason,
			firstTry: prismWSFrame("first question", ""),
			second:   `{"type":"response.create","model":"gpt-6.1-sol","previous_response_id":"r1","input":[` + image + `]}`},
		{name: "later_turn_session_down", retry: true, reason: PrismSessionUnavailableReason,
			firstTry: prismWSFrame("first question", ""),
			second:   prismWSFrame("second question", "r1"),
			prepare:  func(h *prismGatewayHarness) { h.runtimeMode.Store(prismRuntimeNotReady) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPrismGatewayHarness(t, nil, nil)
			bridge := newPrismWSBridge(t, h, false, prismWSTurnResponse("r1"))

			bridge.send(test.firstTry)
			if test.retry {
				bridge.completed()
				require.Len(t, h.upstream.bodies, 1)
				if test.prepare != nil {
					test.prepare(h)
				}
				bridge.send(test.second)
			}
			err := bridge.finish()

			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, test.reason, failoverErr.Reason)
			if test.reason == PrismSessionUnavailableReason {
				require.True(t, failoverErr.ShouldReportAccountScheduleFailure())
			} else {
				require.True(t, failoverErr.IsPrismRequestUnsupported())
				require.False(t, failoverErr.ShouldReportAccountScheduleFailure())
			}
			wantCalls := 0
			if test.retry {
				wantCalls = 1
			}
			require.Len(t, h.upstream.bodies, wantCalls, "the failing turn never reaches the sidecar")
			if test.retry {
				retryPayload, retryCurrentTurn := OpenAIWSCurrentTurnRetryPayload(err)
				require.True(t, retryCurrentTurn, "a later turn is retried on the replacement account")
				require.False(t, gjson.GetBytes(retryPayload, "previous_response_id").Exists())
				input := gjson.GetBytes(retryPayload, "input").Array()
				require.GreaterOrEqual(t, len(input), 3, "the replacement account receives the whole conversation")
				require.Equal(t, "answer-r1", input[1].Get("content.0.text").String())
			}
			// Whatever the successful first turn delivered has been read already, so
			// any further frame would be output of the failed turn.
			readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for {
				_, event, readErr := bridge.client.Read(readCtx)
				if readErr != nil {
					break
				}
				t.Fatalf("the failed turn wrote a frame to the client: %s", event)
			}
		})
	}
}

func TestPrismWebSocketBridgeDoesNotServeAccountsThatAreNotBridged(t *testing.T) {
	h := newPrismGatewayHarness(t, nil, nil)
	h.account.Extra["openai_apikey_responses_websockets_v2_mode"] = "off"
	bridge := newPrismWSBridge(t, h, false, prismWSTurnResponse("r1"))
	bridge.send(prismWSFrame("hello", ""))
	err := bridge.finish()
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Empty(t, h.upstream.bodies, "an account without the bridge mode never reaches the sidecar over WS")
}

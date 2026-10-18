package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const httpEncryptedTestInput = `[{"type":"reasoning","encrypted_content":"reasoning-ciphertext","summary":[{"type":"summary_text","text":"retain summary"}]},{"type":"message","role":"user","content":"retain user task"},{"type":"function_call","call_id":"call_1","name":"run","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"retain tool result"}]`

func httpEncryptedTestError(code, message string, bare bool) string {
	if bare {
		return fmt.Sprintf(`{"type":"error","error":{"code":%q,"message":%q}}`, code, message)
	}
	return fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_rejected","status":"failed","error":{"code":%q,"message":%q}}}`, code, message)
}

func httpEncryptedTestSSE(payload string) string {
	return "event: " + gjson.Get(payload, "type").String() + "\ndata: " + payload + "\n\n"
}

func httpEncryptedTestResponse(status int, body string) *http.Response {
	contentType := "application/json"
	if strings.HasPrefix(body, "event:") {
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func runHTTPEncryptedTestForward(t *testing.T, passthrough, stream bool, input string, responses ...*http.Response) (*httpUpstreamSequenceRecorder, *httptest.ResponseRecorder, *gin.Context, error) {
	t.Helper()
	return runHTTPEncryptedTestForwardAccount(t, AccountTypeAPIKey, passthrough, stream, input, responses...)
}

func runHTTPEncryptedTestForwardAccount(t *testing.T, accountType string, passthrough, stream bool, input string, responses ...*http.Response) (*httpUpstreamSequenceRecorder, *httptest.ResponseRecorder, *gin.Context, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"instructions":"test","previous_response_id":"resp_prior","input":%s}`, stream, input))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	upstream := &httpUpstreamSequenceRecorder{responses: responses}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID: 216, Name: "encrypted-test", Platform: PlatformOpenAI, Type: accountType,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "test-api-key", "access_token": "test-access-token", "chatgpt_account_id": "test-account"},
		Extra:       map[string]any{"openai_passthrough": false},
	}
	var err error
	if passthrough {
		_, err = svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "gpt-5.6-sol", false, nil, stream, time.Now())
	} else {
		_, err = svc.Forward(context.Background(), c, account, body)
	}
	require.Equal(t, passthrough, c.GetBool("openai_passthrough"), "fixture must exercise the requested response path")
	return upstream, rec, c, err
}

func TestOpenAIHTTPEncryptedRecoveryErrorsAndBodySafety(t *testing.T) {
	preciseMessage := "The encrypted content for item rs_1 could not be verified. Reason: encrypted content hydration failed: Encrypted content could not be decrypted or parsed."
	for _, tc := range []struct {
		name, code, message, input string
		want                       bool
	}{
		{"reasoning", "invalid_encrypted_content", "invalid", httpEncryptedTestInput, true},
		{"precise signature", "thinking_signature_invalid", preciseMessage, httpEncryptedTestInput, true},
		{"other signature", "thinking_signature_invalid", "signature mismatch", httpEncryptedTestInput, false},
		{"unrelated", "invalid_request_error", preciseMessage, httpEncryptedTestInput, false},
		{"task item error", "invalid_encrypted_content", strings.ReplaceAll(preciseMessage, "rs_1", "amsg_123"), httpEncryptedTestInput, false},
		{"task ciphertext", "invalid_encrypted_content", "invalid", `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"agent_message","encrypted_content":"task secret","content":"must stay"}]`, false},
		{"nested task ciphertext", "invalid_encrypted_content", "invalid", `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"message","content":[{"type":"agent_message","encrypted_content":"task secret"}]}]`, false},
		{"encrypted function arguments", "invalid_encrypted_content", "invalid", `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"function_call","encrypted_function_args":["message"],"arguments":"{\"message\":\"task cipher\"}"}]`, false},
		{"legacy collaboration arguments", "invalid_encrypted_content", "invalid", `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{\"message\":\"task cipher\"}"}]`, false},
		{"opaque part data", "invalid_encrypted_content", "invalid", `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"agent_message","content":[{"type":"encrypted_content","data":"task cipher"}]}]`, false},
		{"no ciphertext", "invalid_encrypted_content", "invalid", `[{"type":"message","content":"keep"}]`, false},
		{"single reasoning", "invalid_encrypted_content", "invalid", `{"type":"reasoning","encrypted_content":"cipher","summary":[]}`, true},
		{"compaction", "invalid_encrypted_content", "invalid", `[{"type":"compaction","encrypted_content":"cipher"},{"type":"message","content":"keep"}]`, false},
		{"compaction summary", "invalid_encrypted_content", "invalid", `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"compaction_summary","encrypted_content":"sole task history"}]`, false},
		{"plaintext compaction", "invalid_encrypted_content", "invalid", `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"compaction","summary":"sole task history"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"input":` + tc.input + `,"previous_response_id":"resp_prior"}`)
			before := string(body)
			state := &openAIHTTPEncryptedRecovery{body: body}
			signal := state.prepare([]byte(httpEncryptedTestError(tc.code, tc.message, false)))
			require.Equal(t, tc.want, signal != nil)
			require.Equal(t, before, string(body), "recovery must not mutate the original request")
			if signal != nil {
				require.NotContains(t, string(signal.body), "encrypted_content")
				require.Equal(t, "resp_prior", gjson.GetBytes(signal.body, "previous_response_id").String())
				require.Nil(t, state.prepare([]byte(httpEncryptedTestError(tc.code, tc.message, false))), "one budget shared across error forms")
			}
		})
	}
}

func TestOpenAIHTTPEncryptedRecoveryBeforeOutput(t *testing.T) {
	metadata := httpEncryptedTestSSE(`{"type":"response.created","response":{"id":"resp_rejected","status":"in_progress"}}`)
	completed := httpEncryptedTestSSE(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","model":"gpt-5.6-sol","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, bare := range []bool{false, true} {
				t.Run(fmt.Sprintf("passthrough=%t/stream=%t/bare=%t", passthrough, stream, bare), func(t *testing.T) {
					failed := metadata + httpEncryptedTestSSE(httpEncryptedTestError("invalid_encrypted_content", "bad encrypted reasoning", bare))
					upstream, rec, c, err := runHTTPEncryptedTestForward(t, passthrough, stream, httpEncryptedTestInput,
						httpEncryptedTestResponse(http.StatusOK, failed), httpEncryptedTestResponse(http.StatusOK, completed))
					require.NoError(t, err)
					require.Equal(t, 2, upstream.callCount)
					require.Contains(t, rec.Body.String(), "resp_ok")
					require.NotContains(t, rec.Body.String(), "resp_rejected", "discard failed attempt's metadata")
					require.NotContains(t, rec.Body.String(), "invalid_encrypted_content")
					require.NotContains(t, string(upstream.bodies[1]), "encrypted_content")
					require.Equal(t, "retain summary", gjson.GetBytes(upstream.bodies[1], "input.0.summary.0.text").String())
					require.Equal(t, "resp_prior", gjson.GetBytes(upstream.bodies[1], "previous_response_id").String())
					for _, text := range []string{"retain user task", "retain tool result"} {
						require.Contains(t, string(upstream.bodies[1]), text)
					}
					require.Equal(t, upstream.reqs[0].Header.Get("Authorization"), upstream.reqs[1].Header.Get("Authorization"), "must stay on the same account")
					events, ok := c.Get(OpsUpstreamErrorsKey)
					require.True(t, ok)
					typedEvents, ok := events.([]*OpsUpstreamErrorEvent)
					require.True(t, ok)
					require.Equal(t, "invalid_encrypted_content", typedEvents[0].Reason)
				})
			}
		}
	}
}

func TestOpenAIHTTPEncryptedRecoveryBoundedAcrossHTTPAndSSE(t *testing.T) {
	failed := httpEncryptedTestError("invalid_encrypted_content", "still invalid", false)
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			upstream, rec, _, err := runHTTPEncryptedTestForward(t, passthrough, true, httpEncryptedTestInput,
				httpEncryptedTestResponse(http.StatusBadRequest, `{"error":{"code":"invalid_encrypted_content","message":"invalid"}}`),
				httpEncryptedTestResponse(http.StatusOK, httpEncryptedTestSSE(failed)))
			require.Error(t, err)
			require.Equal(t, 2, upstream.callCount)
			require.Contains(t, rec.Body.String(), "invalid_encrypted_content")
		})
	}
}

func TestOpenAIHTTPEncryptedRecoveryOAuthBeforeOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			failed := httpEncryptedTestSSE(httpEncryptedTestError("thinking_signature_invalid", "The encrypted content could not be verified: could not be decrypted or parsed", false))
			completed := httpEncryptedTestSSE(`{"type":"response.completed","response":{"id":"resp_oauth_ok","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
			upstream, rec, _, err := runHTTPEncryptedTestForwardAccount(t, AccountTypeOAuth, passthrough, true, httpEncryptedTestInput,
				httpEncryptedTestResponse(http.StatusOK, failed), httpEncryptedTestResponse(http.StatusOK, completed))
			require.NoError(t, err)
			require.Equal(t, 2, upstream.callCount)
			require.Contains(t, rec.Body.String(), "resp_oauth_ok")
			require.NotContains(t, string(upstream.bodies[1]), "encrypted_content")
		})
	}
}

func TestOpenAIHTTPEncryptedRecoveryRefusesReplayAfterOutputOrTaskFailure(t *testing.T) {
	delta := httpEncryptedTestSSE(`{"type":"response.output_text.delta","delta":"already delivered"}`)
	toolCall := httpEncryptedTestSSE(`{"type":"response.output_item.added","item":{"type":"function_call","name":"run","call_id":"call_new","arguments":"{}"}}`)
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct{ name, prefix, message, input string }{
				{"text output", delta, "invalid", httpEncryptedTestInput},
				{"tool call", toolCall, "invalid", httpEncryptedTestInput},
				{"agent message error", "", "The encrypted content for item amsg_123 could not be verified", httpEncryptedTestInput},
				{"agent ciphertext", "", "invalid", `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"agent_message","encrypted_content":"task"}]`},
			} {
				t.Run(fmt.Sprintf("passthrough=%t/stream=%t/%s", passthrough, stream, tc.name), func(t *testing.T) {
					failed := tc.prefix + httpEncryptedTestSSE(httpEncryptedTestError("invalid_encrypted_content", tc.message, false))
					upstream, _, _, err := runHTTPEncryptedTestForward(t, passthrough, stream, tc.input, httpEncryptedTestResponse(http.StatusOK, failed))
					require.Error(t, err)
					require.Equal(t, 1, upstream.callCount)
				})
			}
		}
	}
}

func TestOpenAIHTTPEncryptedRecoveryKeepaliveAndCommittedOutput(t *testing.T) {
	for _, semantic := range []bool{false, true} {
		t.Run(fmt.Sprint(semantic), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			state := &openAIHTTPEncryptedRecovery{body: []byte(`{"input":` + httpEncryptedTestInput + `}`)}
			c.Set(openAIHTTPEncryptedRecoveryKey, state)
			n, err := c.Writer.WriteString(": keepalive\n\n")
			require.NoError(t, err)
			recordOpenAIStreamKeepaliveBytes(c, n)
			if semantic {
				_, err = c.Writer.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
				require.NoError(t, err)
			}
			err = newOpenAIHTTPEncryptedRecoverySignal(c, []byte(httpEncryptedTestError("invalid_encrypted_content", "invalid", true)), false)
			_, recoverable := asOpenAIHTTPEncryptedRecoverySignal(err)
			require.Equal(t, !semantic, recoverable)
		})
	}
}

func TestOpenAIHTTPEncryptedRejectionNeverRequestsAccountFailover(t *testing.T) {
	for _, payload := range []string{
		httpEncryptedTestError("invalid_encrypted_content", "invalid", false),
		httpEncryptedTestError("invalid_encrypted_content", "The encrypted content for item amsg_1 could not be verified. Please retry.", true),
		`{"type":"error","code":"invalid_encrypted_content","message":"invalid"}`,
		httpEncryptedTestError("thinking_signature_invalid", "The encrypted content could not be verified: could not be decrypted or parsed", false),
	} {
		raw := []byte(payload)
		require.True(t, isOpenAIEncryptedContentRejection(raw))
		require.False(t, openAIStreamFailedEventShouldFailover(raw, extractOpenAISSEErrorMessage(raw)))
		require.False(t, openAIStreamErrorEventShouldFailover(raw, extractOpenAISSEErrorMessage(raw)))
		require.False(t, shouldFailoverOpenAIPassthroughResponse(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, http.StatusBadRequest, raw))
	}
}

func TestOpenAIWSEncryptedRecoveryDoesNotStripTaskFailures(t *testing.T) {
	for _, tc := range []struct{ name, input, message, output string }{
		{"amsg diagnostic", httpEncryptedTestInput, "The encrypted content for item amsg_123 could not be verified", ""},
		{"mixed task content", `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"task ciphertext"}]}]`, "The encrypted content could not be verified", ""},
		{"compaction history", `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"compaction","encrypted_content":"only task history"}]`, "invalid", ""},
		{"terminal output", httpEncryptedTestInput, "invalid", `,"output":[{"type":"web_search_call","status":"completed"}]`},
		{"terminal usage", httpEncryptedTestInput, "invalid", `,"usage":{"output_tokens":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				conn, err := upgrader.Upgrade(w, req, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err := conn.ReadMessage(); err != nil {
					t.Error(err)
					return
				}
				attempts.Add(1)
				failed := httpEncryptedTestError("invalid_encrypted_content", tc.message, true)
				if tc.output != "" {
					failed = strings.TrimSuffix(failed, "}") + tc.output + "}"
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(failed))
			}))
			defer server.Close()
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			svc := &OpenAIGatewayService{cfg: cfg, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg)}
			account := &Account{
				ID: 216, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "test-key", "base_url": server.URL},
				Extra:       map[string]any{"responses_websockets_v2_enabled": true},
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			body := []byte(`{"model":"gpt-5.3-codex","stream":false,"input":` + tc.input + `}`)
			_, err := svc.Forward(context.Background(), c, account, body)
			require.Error(t, err)
			require.Equal(t, int32(1), attempts.Load())
			require.False(t, svc.getOpenAIWSStateStore().HasAnySessionInvalidEncryptedContent(), "task failures must not invalidate unrelated reasoning ciphertext")
		})
	}
}

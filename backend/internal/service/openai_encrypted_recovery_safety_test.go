package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIEncryptedRecoveryPreservesCompactionAcrossHTTPAndSSE(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, sse := range []bool{false, true} {
				for _, itemType := range []string{"compaction", "compaction_summary"} {
					t.Run(fmt.Sprintf("passthrough=%t/stream=%t/sse=%t/%s", passthrough, stream, sse, itemType), func(t *testing.T) {
						input := fmt.Sprintf(`[{"type":"reasoning","encrypted_content":"reasoning"},{"type":%q,"encrypted_content":"only remaining task history"}]`, itemType)
						status, failed := http.StatusBadRequest, `{"error":{"code":"invalid_encrypted_content","message":"invalid ciphertext"}}`
						if sse {
							status, failed = http.StatusOK, httpEncryptedTestSSE(httpEncryptedTestError("invalid_encrypted_content", "invalid ciphertext", false))
						}
						upstream, _, _, err := runHTTPEncryptedTestForward(t, passthrough, stream, input, httpEncryptedTestResponse(status, failed))
						require.Error(t, err)
						require.Equal(t, 1, upstream.callCount)
						require.Contains(t, string(upstream.bodies[0]), "only remaining task history")
					})
				}
			}
		}
	}
}

func TestOpenAIEncryptedRecoveryDoesNotReplayOutputInFailureFrame(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, output := range []string{
				`"output":[{"type":"web_search_call","status":"completed"}]`,
				`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"already answered"}]}]`,
				`"output":[],"usage":{"output_tokens":1}`,
				`"output":[],"usage":{"output_tokens":0,"output_tokens_details":{"reasoning_tokens":1}}`,
			} {
				t.Run(fmt.Sprintf("passthrough=%t/stream=%t/%s", passthrough, stream, output), func(t *testing.T) {
					failed := `{"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_encrypted_content","message":"invalid"},` + output + `}}`
					upstream, _, _, err := runHTTPEncryptedTestForward(t, passthrough, stream, httpEncryptedTestInput,
						httpEncryptedTestResponse(http.StatusOK, httpEncryptedTestSSE(failed)))
					require.Error(t, err)
					require.Equal(t, 1, upstream.callCount, "a failed terminal can itself report completed work")
				})
			}
		}
	}
	for _, payload := range []string{
		`{"error":{"code":"invalid_encrypted_content"},"output":[{"type":"web_search_call"}]}`,
		`{"error":{"code":"invalid_encrypted_content"},"usage":{"reasoning_tokens":1}}`,
	} {
		state := &openAIHTTPEncryptedRecovery{body: []byte(`{"input":` + httpEncryptedTestInput + `}`)}
		require.Nil(t, state.prepare([]byte(payload)), "HTTP error bodies also must not replay reported work")
	}
	state := &openAIHTTPEncryptedRecovery{body: []byte(`{"input":` + httpEncryptedTestInput + `}`)}
	require.NotNil(t, state.prepare([]byte(`{"type":"response.failed","response":{"error":{"code":"invalid_encrypted_content"},"output":[],"usage":{"input_tokens":10,"output_tokens":0}}}`)), "input-only usage and an empty output remain recoverable")
}

func TestOpenAIWSHTTPBridgeEncryptedLineageSafety(t *testing.T) {
	for _, sse := range []bool{false, true} {
		for _, tc := range []struct {
			name, input, message, prefix, output string
			wantLineage                          bool
		}{
			{name: "reasoning only", input: httpEncryptedTestInput, message: "invalid", wantLineage: true},
			{name: "amsg diagnostic", input: httpEncryptedTestInput, message: "The encrypted content for item amsg_123 could not be verified"},
			{name: "mixed agent task", input: `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"task ciphertext"}]}]`, message: "invalid"},
			{name: "compaction history", input: `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"compaction","encrypted_content":"sole task history"}]`, message: "invalid"},
			{name: "compaction summary history", input: `[{"type":"reasoning","encrypted_content":"reasoning"},{"type":"compaction_summary","encrypted_content":"sole task history"}]`, message: "invalid"},
			{name: "terminal output", input: httpEncryptedTestInput, message: "invalid", output: `,"output":[{"type":"web_search_call","status":"completed"}]`},
			{name: "output usage", input: httpEncryptedTestInput, message: "invalid", output: `,"usage":{"output_tokens":2}`},
			{name: "after output", input: httpEncryptedTestInput, message: "invalid", prefix: httpEncryptedTestSSE(`{"type":"response.output_text.delta","delta":"already answered"}`)},
		} {
			if !sse && tc.prefix != "" {
				continue
			}
			t.Run(fmt.Sprintf("sse=%t/%s", sse, tc.name), func(t *testing.T) {
				status := http.StatusBadRequest
				failed := fmt.Sprintf(`{"type":"error","error":{"code":"invalid_encrypted_content","message":%q}%s}`, tc.message, tc.output)
				if sse {
					status, failed = http.StatusOK, tc.prefix+httpEncryptedTestSSE(failed)
				}
				upstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{httpEncryptedTestResponse(status, failed)}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{ID: 216, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}
				payload := []byte(`{"type":"response.create","model":"gpt-5.5","input":` + tc.input + `}`)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
				c.Set(openAIWSIngressSessionHashContextKey, "bridge-encrypted-lineage-test")
				var delivered []string
				_, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "fixture-token", payload, len(payload), "gpt-5.5", "", "", "", "", 1, func(message []byte) error {
					delivered = append(delivered, string(message))
					return nil
				})
				require.Error(t, err)
				require.Equal(t, 1, upstream.callCount)
				require.Equal(t, tc.wantLineage, svc.getOpenAIWSStateStore().HasAnySessionInvalidEncryptedContent())
				require.Contains(t, strings.Join(delivered, "\n"), tc.message, "preserve the terminal error instead of silently replaying the task")
			})
		}
	}
}

func TestOpenAIEncryptedRecoveryWaitsForFailedAfterBareError(t *testing.T) {
	bare := httpEncryptedTestSSE(httpEncryptedTestError("invalid_encrypted_content", "invalid", true))
	failed := func(extra string) string {
		return httpEncryptedTestSSE(`{"type":"response.failed","response":{"id":"resp_rejected","status":"failed","error":{"code":"invalid_encrypted_content","message":"invalid"}` + extra + `}}`)
	}
	completed := httpEncryptedTestSSE(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, body string
				wantRetry  bool
			}{
				{"failed reports tool call", bare + failed(`,"output":[{"type":"web_search_call","status":"completed"}]`), false},
				{"failed reports output tokens", bare + failed(`,"output":[],"usage":{"input_tokens":10,"output_tokens":3}`), false},
				{"failed names task item", bare + httpEncryptedTestSSE(httpEncryptedTestError("invalid_encrypted_content", "The encrypted content for item amsg_123 could not be verified", false)), false},
				{"failed without output", bare + failed(`,"output":[],"usage":{"input_tokens":10,"output_tokens":0}`), true},
				{"bare error at clean EOF", bare, true},
			} {
				t.Run(fmt.Sprintf("passthrough=%t/stream=%t/%s", passthrough, stream, tc.name), func(t *testing.T) {
					upstream, rec, _, err := runHTTPEncryptedTestForwardAccount(t, AccountTypeOAuth, passthrough, stream, httpEncryptedTestInput,
						httpEncryptedTestResponse(http.StatusOK, tc.body), httpEncryptedTestResponse(http.StatusOK, completed))
					if !tc.wantRetry {
						require.Error(t, err)
						require.Equal(t, 1, upstream.callCount, "the frame after a bare error decides whether work already ran")
						return
					}
					require.NoError(t, err)
					require.Equal(t, 2, upstream.callCount)
					require.Contains(t, rec.Body.String(), "resp_ok")
					require.NotContains(t, string(upstream.bodies[1]), "encrypted_content")
				})
			}
		}
	}
}

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// Legacy namespace flattening removes the only recognizable collaboration
// identity on the upstream wire. With no encrypted_function_args marker,
// ownership can only be learned after restoring the client-visible identity.
func TestOpenAIEncryptedContentHTTPFlattenedCollaborationBindsBeforeDelivery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const apiKeyID int64 = 73
	const cipher = "synthetic-flattened-task-cipher"
	groupID := int64(9)
	item := `{"type":"function_call","id":"fc_flattened","call_id":"call_flattened","name":"collaboration__spawn_agent","arguments":"{\"task_name\":\"worker\",\"message\":\"` + cipher + `\"}"}`
	terminal := `{"id":"resp_flattened","object":"response","created_at":1,"status":"completed","model":"gpt-5.6-sol","output":[` + item + `],"usage":{"input_tokens":1,"output_tokens":1}}`
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%t/stream=%t", passthrough, stream), func(t *testing.T) {
				account := newOpenAIOAuthNamespaceTestAccount()
				account.GroupIDs = []int64{groupID}
				account.Extra = map[string]any{"openai_responses_flatten_namespaces": true, "openai_passthrough": passthrough}
				// OAuth requests use upstream SSE even when the client asks for JSON.
				wire := httpEncryptedTestSSE(`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`) +
					httpEncryptedTestSSE(`{"type":"response.completed","response":`+terminal+`}`)
				upstream := &httpUpstreamRecorder{responses: []*http.Response{httpEncryptedTestResponse(http.StatusOK, wire)}}
				svc := newOpenAIRejectedFieldTestService(upstream)
				svc.cache = &encryptedAffinityTestCache{}
				request := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"instructions":"test","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]}],"input":[{"role":"user","content":"start task"}]}`, stream))
				ctx := WithOpenAIEncryptedContentAffinity(context.Background(), apiKeyID, request)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(request)).WithContext(ctx)
				c.Set("api_key", &APIKey{ID: apiKeyID, GroupID: &groupID})
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				sawTask := false
				c.Writer = &encryptedContentObservingWriter{ResponseWriter: c.Writer, beforeWrite: func(payload []byte) {
					if !bytes.Contains(payload, []byte(cipher)) {
						return
					}
					owner, err := svc.getOpenAIWSStateStore().GetEncryptedContentAccount(ctx, groupID, apiKeyID, openAIEncryptedContentDigest(cipher))
					require.NoError(t, err)
					require.Equal(t, account.ID, owner, "restored collaboration task must be bound before delivery")
					require.NotContains(t, string(payload), "collaboration__spawn_agent")
					sawTask = true
				}}

				_, err := svc.Forward(ctx, c, account, request)
				require.NoError(t, err)
				require.True(t, sawTask)
				require.Equal(t, passthrough, c.GetBool("openai_passthrough"))
				require.Len(t, upstream.bodies, 1)
				require.True(t, gjson.GetBytes(upstream.bodies[0], `tools.#(name=="collaboration__spawn_agent")`).Exists(), "fixture must exercise real request flattening")
				require.Contains(t, recorder.Body.String(), `"namespace":"collaboration"`)
				require.Contains(t, recorder.Body.String(), `"name":"spawn_agent"`)
				require.NotContains(t, recorder.Body.String(), "encrypted_function_args", "missing metadata is intentionally preserved")

				child := encryptedAffinityInput(t, cipher)
				require.NoError(t, svc.ValidateOpenAIEncryptedContentAccount(ctx, &groupID, apiKeyID, account.ID, child))
				requireEncryptedAffinityCode(t, svc.ValidateOpenAIEncryptedContentAccount(ctx, &groupID, apiKeyID, account.ID+1, child), "encrypted_content_account_mismatch")
			})
		}
	}
}

// response.inject is not a new turn, so a response.create-only check would
// relay this encrypted task to the wrong account on an established connection.
func TestOpenAIEncryptedContentWSPassthroughRejectsForeignInject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const apiKeyID int64 = 74
	const foreignCipher = "synthetic-foreign-task-cipher"
	groupID := int64(10)
	for _, messageType := range []coderws.MessageType{coderws.MessageText, coderws.MessageBinary} {
		t.Run(fmt.Sprintf("frame=%d", messageType), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			upstream := newStagedPassthroughConn()
			account := passthroughLifecycleAccount()
			account.GroupIDs = []int64{groupID}
			svc := newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream)
			svc.cache = &encryptedAffinityTestCache{}
			foreignObserver := svc.NewOpenAIEncryptedContentObserver(ctx, &groupID, apiKeyID, account.ID+1)
			require.NoError(t, foreignObserver.Observe(encryptedAffinityProducerItem(t, foreignCipher, "missing")))
			server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
				c.Set("api_key", &APIKey{ID: apiKeyID, GroupID: &groupID})
				return nil
			})
			defer server.Close()
			client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-5.6-sol","stream":true,"input":[{"role":"user","content":"hello"}]}`)
			defer func() { _ = client.CloseNow() }()
			first := requirePassthroughUpstreamWrite(t, upstream, time.Second)
			require.Equal(t, "response.create", gjson.GetBytes(first, "type").String())
			upstream.Send(`{"type":"response.created","response":{"id":"resp_live","status":"in_progress"}}`)
			created, err := readPassthroughLifecycleFrame(t, client, time.Second)
			require.NoError(t, err)
			require.Equal(t, "response.created", gjson.GetBytes(created, "type").String())

			inject := []byte(`{"type":"response.inject","response_id":"resp_live","input":[{"type":"agent_message","id":"amsg_foreign","content":[{"type":"encrypted_content","encrypted_content":"` + foreignCipher + `"}]}]}`)
			writeCtx, cancelWrite := context.WithTimeout(ctx, time.Second)
			err = client.Write(writeCtx, messageType, inject)
			cancelWrite()
			require.NoError(t, err)
			// Read the close frame so graceful relay teardown does not wait for
			// the peer handshake timeout.
			_, err = readPassthroughLifecycleFrame(t, client, time.Second)
			require.Error(t, err)
			require.Equal(t, coderws.StatusTryAgainLater, coderws.CloseStatus(err), "send a retryable close before relay cancellation closes the socket")
			require.Contains(t, err.Error(), "Reconnect")
			select {
			case err := <-serverErr:
				requireEncryptedAffinityCode(t, err, "encrypted_content_account_mismatch")
			case <-time.After(3 * time.Second):
				t.Fatal("foreign encrypted injection did not terminate the relay")
			}
			select {
			case forwarded := <-upstream.writes:
				t.Fatalf("foreign response.inject reached upstream: %s", forwarded)
			default:
			}
		})
	}
}

// A failed ownership write must prevent delivery of the item that could start
// a child agent. This detects moving observation after the actual socket write.
func TestOpenAIEncryptedContentWSPassthroughBindFailurePreventsDelivery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const apiKeyID int64 = 75
	groupID := int64(11)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := newStagedPassthroughConn()
	account := passthroughLifecycleAccount()
	account.GroupIDs = []int64{groupID}
	svc := newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream)
	svc.cache = &encryptedAffinityTestCache{writeErr: errors.New("synthetic cache claim failure")}
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
		c.Set("api_key", &APIKey{ID: apiKeyID, GroupID: &groupID})
		return nil
	})
	defer server.Close()
	client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-5.6-sol","stream":true}`)
	defer func() { _ = client.CloseNow() }()
	requirePassthroughUpstreamWrite(t, upstream, time.Second)
	upstream.Send(string(encryptedAffinityProducerItem(t, "synthetic-undeliverable-task-cipher", "missing")))
	payload, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.Error(t, err, "a task must never be delivered when its producer binding fails")
	require.Equal(t, coderws.StatusTryAgainLater, coderws.CloseStatus(err))
	require.Contains(t, err.Error(), "routing is temporarily unavailable")
	require.False(t, strings.Contains(string(payload), "synthetic-undeliverable-task-cipher"))
	select {
	case err := <-serverErr:
		requireEncryptedAffinityCode(t, err, "encrypted_content_affinity_unavailable")
	case <-time.After(3 * time.Second):
		t.Fatal("ownership failure did not terminate the relay")
	}
}

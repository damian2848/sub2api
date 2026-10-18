package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type encryptedContentObservingWriter struct {
	gin.ResponseWriter
	beforeWrite func([]byte)
}

func (w *encryptedContentObservingWriter) Write(body []byte) (int, error) {
	w.beforeWrite(body)
	return w.ResponseWriter.Write(body)
}

// Exercise the real output writer and scheduler together. A child can start as
// soon as item.done arrives, before the parent's terminal event is delivered.
func TestOpenAIEncryptedContentTransportBindsBeforeDeliveryAndRoutesChild(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const apiKeyID int64 = 65
	const cipher = "synthetic+encrypted/task=="
	groupID := int64(3)
	item := `{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","namespace":"collaboration","name":"spawn_agent","arguments":"{\"task_name\":\"worker\",\"message\":\"` + cipher + `\"}","encrypted_function_args":["message"]}`
	terminal := `{"id":"resp_fixture","object":"response","created_at":1,"status":"completed","model":"gpt-5.6-sol","output":[` + item + `],"usage":{"input_tokens":1,"output_tokens":1}}`
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, eventOnly := range []bool{false, true} {
				if !stream && eventOnly {
					continue
				}
				t.Run(fmt.Sprintf("passthrough=%t/stream=%t/eventOnly=%t", passthrough, stream, eventOnly), func(t *testing.T) {
					producer := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "fixture-key"}}
					producer.Extra = map[string]any{"openai_passthrough": passthrough}
					other := producer
					other.ID = 2
					wire := terminal
					if stream {
						wire = httpEncryptedTestSSE(`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`) +
							httpEncryptedTestSSE(`{"type":"response.completed","response":`+terminal+`}`)
						if eventOnly {
							wire = strings.ReplaceAll(wire, `"type":"response.output_item.done",`, "")
							wire = strings.ReplaceAll(wire, `"type":"response.completed",`, "")
						}
					}
					upstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{httpEncryptedTestResponse(http.StatusOK, wire)}}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream,
						cache:              &encryptedAffinityTestCache{},
						accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{producer, other}},
						concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
					}
					request := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"input":[{"role":"user","content":"synthetic task"}]}`, stream))
					ctx := WithOpenAIEncryptedContentAffinity(context.Background(), apiKeyID, request)
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(request))).WithContext(ctx)
					c.Request.Header.Set("session_id", "parent-thread")
					c.Set("api_key", &APIKey{ID: apiKeyID, GroupID: &groupID})
					SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
					sawTask := false
					c.Writer = &encryptedContentObservingWriter{ResponseWriter: c.Writer, beforeWrite: func(body []byte) {
						if !bytes.Contains(body, []byte(cipher)) {
							return
						}
						owner, err := svc.getOpenAIWSStateStore().GetEncryptedContentAccount(ctx, groupID, apiKeyID, openAIEncryptedContentDigest(cipher))
						require.NoError(t, err)
						require.Equal(t, producer.ID, owner, "ownership must exist before task bytes reach the client")
						sawTask = true
					}}
					var err error
					if passthrough {
						_, err = svc.forwardOpenAIPassthrough(ctx, c, &producer, request, request, "gpt-5.6-sol", false, nil, stream, time.Now())
					} else {
						_, err = svc.Forward(ctx, c, &producer, request)
					}
					require.NoError(t, err)
					require.True(t, sawTask)

					child := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"agent_message","id":"amsg_fixture","content":[{"type":"input_text","text":"Task envelope"},{"type":"encrypted_content","encrypted_content":"` + cipher + `"}]}]}`)
					childCtx := WithOpenAIEncryptedContentAffinity(context.Background(), apiKeyID, child)
					selection, decision, err := svc.SelectAccountWithScheduler(childCtx, &groupID, "", "different-child-thread", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
					require.NoError(t, err)
					require.NotNil(t, selection)
					require.Equal(t, producer.ID, selection.Account.ID)
					require.Equal(t, openAIAccountScheduleLayerEncryptedContent, decision.Layer)
					if selection.ReleaseFunc != nil {
						selection.ReleaseFunc()
					}
					selection, _, err = svc.SelectAccountWithScheduler(childCtx, &groupID, "", "different-child-thread", "gpt-5.6-sol", map[int64]struct{}{producer.ID: {}}, OpenAIUpstreamTransportAny, false)
					require.Error(t, err, "an excluded producer must never fall back to the other account")
					require.Nil(t, selection)
					require.Contains(t, string(child), cipher, "the original task is retained")
				})
			}
		}
	}
}

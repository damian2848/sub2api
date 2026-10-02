package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func prismStreamTestAccount() *Account {
	return &Account{ID: 201, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{"provider_preset": PrismProviderPreset, PrismSourceAccountKey: int64(32)}}
}

type prismStreamTestResult struct {
	responseID   string
	firstTokenMs *int
}

func runPrismHTTPStreamTest(svc *OpenAIGatewayService, c *gin.Context, body io.ReadCloser, passthrough bool) (prismStreamTestResult, error) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"Content-Type": {"text/event-stream"}, "X-Request-Id": {"prism-request"},
	}, Body: body}
	if passthrough {
		result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, prismStreamTestAccount(), time.Now(), "gpt-5.6-sol", "gpt-5.6-sol")
		if result == nil {
			return prismStreamTestResult{}, err
		}
		return prismStreamTestResult{result.responseID, result.firstTokenMs}, err
	}
	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, prismStreamTestAccount(), time.Now(), "gpt-5.6-sol", "gpt-5.6-sol")
	if result == nil {
		return prismStreamTestResult{}, err
	}
	return prismStreamTestResult{result.responseID, result.firstTokenMs}, err
}

func newPrismHTTPStreamTestContext() (*OpenAIGatewayService, *gin.Context, *openAIResponseFlushRecorder) {
	recorder := newOpenAIResponseFlushRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	svc := &OpenAIGatewayService{toolCorrector: NewCodexToolCorrector(), cfg: &config.Config{Gateway: config.GatewayConfig{
		MaxLineSize: defaultMaxLineSize, OpenAIFirstOutputTimeoutSeconds: 5, StreamKeepaliveInterval: 1,
	}}}
	return svc, c, recorder
}

const prismStreamCreated = "event: response.created\ndata: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_prism_stream","model":"gpt-5.6-sol"}}` + "\n\n"
const prismStreamInProgress = "event: response.in_progress\ndata: " + `{"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_prism_stream"}}` + "\n\n"

func TestPrismHTTPStreamLifecycleIsEarlyWithoutChangingTTFT(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, c, recorder := newPrismHTTPStreamTestContext()
				pr, pw := io.Pipe()
				defer pr.Close()
				defer pw.Close()
				finish := make(chan struct{})
				go func() {
					_, _ = io.WriteString(pw, prismStreamCreated+prismStreamInProgress)
					<-finish
					_, _ = io.WriteString(pw, `data: {"type":"response.output_text.delta","sequence_number":2,"delta":"answer"}`+"\n\n"+
						`data: {"type":"response.completed","sequence_number":3,"response":{"id":"resp_prism_stream","usage":{"input_tokens":2,"output_tokens":1}}}`+"\n\n")
					_ = pw.Close()
				}()
				resultCh := make(chan prismStreamTestResult, 1)
				errCh := make(chan error, 1)
				go func() {
					result, err := runPrismHTTPStreamTest(svc, c, pr, passthrough)
					resultCh <- result
					errCh <- err
				}()
				synctest.Wait()
				body, flushes := recorder.snapshot()
				require.Equal(t, prismStreamCreated+prismStreamInProgress, body)
				require.Len(t, flushes, 2)
				require.True(t, IsPrismStreamCommitted(c))
				require.False(t, IsResponseCommitted(c), "lifecycle must not suppress a later terminal error")
				require.Equal(t, "prism-request", recorder.Header().Get("X-Request-Id"))
				time.Sleep(1500 * time.Millisecond)
				synctest.Wait()
				body, _ = recorder.snapshot()
				require.True(t, strings.Contains(body, ":\n\n") || strings.Contains(body, ": keepalive\n\n"), "waiting for native completion keeps the public stream alive")
				require.NotContains(t, body, "output_text")
				close(finish)
				require.NoError(t, <-errCh)
				result := <-resultCh
				require.Equal(t, "resp_prism_stream", result.responseID)
				require.NotNil(t, result.firstTokenMs)
				require.GreaterOrEqual(t, *result.firstTokenMs, 1500, "lifecycle is not a semantic token")
				body, flushes = recorder.snapshot()
				require.Equal(t, 1, strings.Count(body, "event: response.created"))
				require.Equal(t, 1, strings.Count(body, "event: response.in_progress"))
				for _, flushed := range flushes {
					require.True(t, strings.HasSuffix(flushed, "\n\n"), "flush must respect the SSE boundary")
				}
			})
		})
	}
}

func TestPrismHTTPStreamPartialEventTimeoutDoesNotReplayOrLeak(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, eventType := range []string{"response.in_progress", "response.output_text.delta", "response.completed"} {
			name := "native/" + eventType
			if passthrough {
				name = "passthrough/" + eventType
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					svc, c, recorder := newPrismHTTPStreamTestContext()
					svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 2
					pr, pw := io.Pipe()
					defer pw.Close()
					body := &firstOutputCloseTrackingBody{ReadCloser: pr, closed: make(chan struct{})}
					go func() {
						_, _ = io.WriteString(pw, prismStreamCreated)
						_, _ = io.WriteString(pw, "event: "+eventType+"\ndata: "+
							`{"type":"`+eventType+`","sequence_number":1,"delta":"`+strings.Repeat("x", 68106)+`"}`+"\n")
					}()
					result, err := runPrismHTTPStreamTest(svc, c, body, passthrough)
					require.Error(t, err)
					var failoverErr *UpstreamFailoverError
					if errors.As(err, &failoverErr) {
						require.False(t, failoverErr.SafeToFailoverAfterWrite)
					}
					require.True(t, IsPrismStreamCommitted(c))
					require.True(t, IsResponseCommitted(c), "the terminal error was communicated")
					require.Nil(t, result.firstTokenMs)
					wire, _ := recorder.snapshot()
					require.NotContains(t, wire, strings.Repeat("x", 100))
					require.Equal(t, 1, strings.Count(wire, "event: response.created"))
					dataLines := []gjson.Result{}
					for _, line := range strings.Split(wire, "\n") {
						if data, ok := extractOpenAISSEDataLine(line); ok {
							require.True(t, gjson.Valid(data))
							dataLines = append(dataLines, gjson.Parse(data))
						}
					}
					require.Len(t, dataLines, 2)
					require.Equal(t, int64(1), dataLines[1].Get("sequence_number").Int(), "discarded partial events must not consume a public sequence number")
					if passthrough {
						require.Equal(t, "response.failed", dataLines[1].Get("type").String())
						require.Equal(t, "resp_prism_stream", dataLines[1].Get("response.id").String())
					} else {
						require.Equal(t, "error", dataLines[1].Get("type").String())
						require.Equal(t, "first_output_timeout", dataLines[1].Get("code").String())
						require.NotEmpty(t, dataLines[1].Get("message").String())
						require.False(t, dataLines[1].Get("error").Exists())
					}
					select {
					case <-body.closed:
					default:
						t.Fatal("timeout did not close the upstream reader")
					}
				})
			})
		}
	}
}

func TestPrismHTTPStreamFailureAfterLifecycleKeepsTheExistingResponse(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, failure := range []string{
			`data: {"type":"response.failed","sequence_number":2,"response":{"id":"resp_prism_stream","status":"failed","error":{"code":"server_error","message":"native failure"}}}` + "\n\n",
			`data: {"type":"error","sequence_number":2,"code":"server_error","message":"native failure","param":null}` + "\n\n",
			"",
		} {
			svc, c, recorder := newPrismHTTPStreamTestContext()
			result, err := runPrismHTTPStreamTest(svc, c, io.NopCloser(strings.NewReader(prismStreamCreated+prismStreamInProgress+failure)), passthrough)
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr), "a public response must not be replayed")
			require.Equal(t, "resp_prism_stream", result.responseID)
			body, _ := recorder.snapshot()
			require.Equal(t, 1, strings.Count(body, "event: response.created"))
			require.True(t, strings.Contains(body, "response.failed") || strings.Contains(body, `"type":"error"`), "passthrough=%t failure=%q body=%s", passthrough, failure, body)
		}
	}
}

func TestPrismFirstOutputTimeoutBeforeLifecycleRemainsSafeToRetry(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	err := svc.newOpenAIFirstOutputTimeoutError(context.Background(), c, prismStreamTestAccount(), nil, "", time.Now(), "gpt-5.6-sol", "", time.Second, "headers", nil)
	require.True(t, err.SafeToFailoverAfterWrite)
	MarkPrismStreamCommitted(c)
	err = svc.newOpenAIFirstOutputTimeoutError(context.Background(), c, prismStreamTestAccount(), nil, "", time.Now(), "gpt-5.6-sol", "", time.Second, "semantic_output", nil)
	require.False(t, err.SafeToFailoverAfterWrite)
}

func TestPrismHTTPStreamEOFSealsPartialEventBeforeTerminalError(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, eventType := range []string{"response.in_progress", "response.output_text.delta"} {
			svc, c, recorder := newPrismHTTPStreamTestContext()
			partial := `data: {"type":"` + eventType + `","sequence_number":1,"delta":"answer"}` + "\n"
			_, err := runPrismHTTPStreamTest(svc, c, io.NopCloser(strings.NewReader(prismStreamCreated+partial)), passthrough)
			require.Error(t, err)
			wire, _ := recorder.snapshot()
			var sequences []int64
			for _, event := range strings.Split(wire, "\n\n") {
				var dataLines []string
				for _, line := range strings.Split(event, "\n") {
					if data, ok := extractOpenAISSEDataLine(line); ok {
						dataLines = append(dataLines, data)
					}
				}
				if len(dataLines) > 0 {
					data := strings.Join(dataLines, "\n")
					require.True(t, gjson.Valid(data), "passthrough=%t event=%q", passthrough, event)
					sequences = append(sequences, gjson.Get(data, "sequence_number").Int())
				}
			}
			require.Equal(t, []int64{0, 1, 2}, sequences)
		}
	}
}

func TestPrismHTTPStreamDoneWithoutBoundaryTimesOutWithTerminalFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, c, recorder := newPrismHTTPStreamTestContext()
		svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 2
		pr, pw := io.Pipe()
		defer pw.Close()
		go func() { _, _ = io.WriteString(pw, prismStreamCreated+"data: [DONE]\n") }()
		_, err := runPrismHTTPStreamTest(svc, c, pr, true)
		require.Error(t, err)
		wire, _ := recorder.snapshot()
		require.NotContains(t, wire, "[DONE]")
		require.Contains(t, wire, "response.failed")
		require.True(t, IsResponseCommitted(c))
	})
}

func TestPrismChatStreamRoleIsEarlyWithoutChangingTTFT(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, c, recorder := newPrismHTTPStreamTestContext()
		pr, pw := io.Pipe()
		defer pr.Close()
		defer pw.Close()
		role := `data: {"id":"chatcmpl_prism","object":"chat.completion.chunk","model":"gpt-5.6-sol","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n"
		finish := make(chan struct{})
		go func() {
			_, _ = io.WriteString(pw, role)
			<-finish
			_, _ = io.WriteString(pw, `data: {"id":"chatcmpl_prism","choices":[{"index":0,"delta":{"content":"answer"}}]}`+"\n\n"+
				`data: {"id":"chatcmpl_prism","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`+"\n\n"+
				"data: [DONE]\n\n")
			_ = pw.Close()
		}()
		resultCh := make(chan *OpenAIForwardResult, 1)
		errCh := make(chan error, 1)
		go func() {
			result, err := svc.streamRawChatCompletions(c, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: pr},
				prismStreamTestAccount(), "gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-sol", nil, nil, time.Now(), openAISilentRefusalMinRequestBodyBytes)
			resultCh <- result
			errCh <- err
		}()
		synctest.Wait()
		wire, flushes := recorder.snapshot()
		require.Equal(t, role, wire, "large requests must expose a complete role event before body text")
		require.Len(t, flushes, 1)
		require.True(t, IsPrismStreamCommitted(c))
		time.Sleep(1500 * time.Millisecond)
		close(finish)
		require.NoError(t, <-errCh)
		result := <-resultCh
		require.NotNil(t, result.FirstTokenMs)
		require.GreaterOrEqual(t, *result.FirstTokenMs, 1500)
		_, flushes = recorder.snapshot()
		for _, flushed := range flushes {
			require.True(t, strings.HasSuffix(flushed, "\n\n"))
		}
	})
}

func TestPrismChatStreamRoleOnlyFailureDoesNotCountTTFTOrReplay(t *testing.T) {
	svc, c, _ := newPrismHTTPStreamTestContext()
	role := `data: {"id":"chatcmpl_prism","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"
	result, err := svc.streamRawChatCompletions(c, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(role))},
		prismStreamTestAccount(), "gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-sol", nil, nil, time.Now(), openAISilentRefusalMinRequestBodyBytes)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Nil(t, result.FirstTokenMs)
	require.True(t, IsPrismStreamCommitted(c))
}

func TestPrismChatStreamErrorIsDeliveredOnlyOnce(t *testing.T) {
	svc, c, recorder := newPrismHTTPStreamTestContext()
	role := `data: {"id":"chatcmpl_prism","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"
	errorEvent := `data: {"error":{"code":"native_failed","type":"upstream_error","message":"Native request failed"}}` + "\n\n"
	result, err := svc.streamRawChatCompletions(c, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(role + errorEvent))},
		prismStreamTestAccount(), "gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-sol", nil, nil, time.Now(), openAISilentRefusalMinRequestBodyBytes)
	require.ErrorContains(t, err, "upstream response failed:")
	require.Nil(t, result.FirstTokenMs)
	require.True(t, IsResponseCommitted(c), "the handler must not append a second error")
	wire, _ := recorder.snapshot()
	require.Equal(t, role+errorEvent, wire)
}

func TestPrismChatStreamPartialTerminalReadErrorDoesNotSucceed(t *testing.T) {
	svc, c, recorder := newPrismHTTPStreamTestContext()
	role := `data: {"id":"chatcmpl_prism","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"
	partial := `data: {"id":"chatcmpl_prism","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n"
	body := &openAIResponseFlushReadError{payload: []byte(role + partial), err: io.ErrUnexpectedEOF}
	result, err := svc.streamRawChatCompletions(c, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body},
		prismStreamTestAccount(), "gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-sol", nil, nil, time.Now(), openAISilentRefusalMinRequestBodyBytes)
	require.Error(t, err)
	require.Nil(t, result.FirstTokenMs)
	wire, _ := recorder.snapshot()
	require.Equal(t, role, wire, "unfinished terminal events are not public")
}

const prismReasoningStreamHead = `event: response.output_item.added` + "\n" + `data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_prism_1","type":"reasoning","summary":[]}}` + "\n\n" +
	`event: response.reasoning_summary_part.added` + "\n" + `data: {"type":"response.reasoning_summary_part.added","sequence_number":3,"item_id":"rs_prism_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}` + "\n\n" +
	`event: response.reasoning_summary_text.delta` + "\n" + `data: {"type":"response.reasoning_summary_text.delta","sequence_number":4,"item_id":"rs_prism_1","output_index":0,"summary_index":0,"delta":"**Planning the page**"}` + "\n\n"

// A reasoning summary is real client output: it reaches the client while Prism is still generating, it is
// Prism's first token, and a later failure is delivered in place instead of failing over to another account.
func TestPrismHTTPStreamReasoningSummaryIsDeliveredBeforeTheAnswer(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, c, recorder := newPrismHTTPStreamTestContext()
				pr, pw := io.Pipe()
				defer pr.Close()
				defer pw.Close()
				finish := make(chan struct{})
				go func() {
					_, _ = io.WriteString(pw, prismStreamCreated+prismStreamInProgress+prismReasoningStreamHead)
					<-finish
					_, _ = io.WriteString(pw, `data: {"type":"response.reasoning_summary_text.done","sequence_number":5,"item_id":"rs_prism_1","output_index":0,"summary_index":0,"text":"**Planning the page**"}`+"\n\n"+
						`data: {"type":"response.output_text.delta","sequence_number":6,"output_index":1,"delta":"answer"}`+"\n\n"+
						`data: {"type":"response.completed","sequence_number":7,"response":{"id":"resp_prism_stream","usage":{"input_tokens":2,"output_tokens":1}}}`+"\n\n")
					_ = pw.Close()
				}()
				resultCh := make(chan prismStreamTestResult, 1)
				errCh := make(chan error, 1)
				go func() {
					result, err := runPrismHTTPStreamTest(svc, c, pr, passthrough)
					resultCh <- result
					errCh <- err
				}()
				synctest.Wait()
				body, _ := recorder.snapshot()
				require.Equal(t, prismStreamCreated+prismStreamInProgress+prismReasoningStreamHead, body,
					"the lifecycle and the first reasoning summary are on the wire before the answer exists")
				require.True(t, IsPrismStreamCommitted(c))
				require.NotContains(t, body, "output_text")
				close(finish)
				require.NoError(t, <-errCh)
				result := <-resultCh
				require.Equal(t, "resp_prism_stream", result.responseID)
				require.NotNil(t, result.firstTokenMs)
				body, flushes := recorder.snapshot()
				require.Equal(t, 1, strings.Count(body, "event: response.created"))
				require.Equal(t, 1, strings.Count(body, "event: response.output_item.added"), "no duplicated or reordered events")
				require.Contains(t, body, `"delta":"answer"`)
				for _, flushed := range flushes {
					require.True(t, strings.HasSuffix(flushed, "\n\n"), "flush must respect the SSE boundary")
				}
			})
		})
	}
}

func TestPrismHTTPStreamFailureAfterReasoningIsNotReplayedOnAnotherAccount(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		svc, c, recorder := newPrismHTTPStreamTestContext()
		failure := `data: {"type":"response.failed","sequence_number":5,"response":{"id":"resp_prism_stream","status":"failed","error":{"code":"server_error","message":"native failure"}}}` + "\n\n"
		_, err := runPrismHTTPStreamTest(svc, c, io.NopCloser(strings.NewReader(
			prismStreamCreated+prismStreamInProgress+prismReasoningStreamHead+failure)), passthrough)
		require.Error(t, err)
		var failoverErr *UpstreamFailoverError
		require.False(t, errors.As(err, &failoverErr), "the client already saw the reasoning, so the request cannot move to another account (passthrough=%t)", passthrough)
		body, _ := recorder.snapshot()
		require.Contains(t, body, "**Planning the page**")
		require.True(t, strings.Contains(body, "response.failed") || strings.Contains(body, `"type":"error"`), body)
	}
}

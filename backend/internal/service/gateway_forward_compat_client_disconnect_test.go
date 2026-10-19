//go:build unit

package service

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// disconnectOnMarkerResponseWriter 在首次写入包含 marker 的数据时返回错误，模拟客户端中途断开。
type disconnectOnMarkerResponseWriter struct {
	*httptest.ResponseRecorder
	marker            string
	failed            bool
	writesAfterError  int
	flushesAfterError int
}

func (writer *disconnectOnMarkerResponseWriter) Write(data []byte) (int, error) {
	if writer.failed {
		writer.writesAfterError++
		return 0, errors.New("client disconnected")
	}
	if strings.Contains(string(data), writer.marker) {
		writer.failed = true
		return 0, errors.New("client disconnected")
	}
	return writer.ResponseRecorder.Write(data)
}

func (writer *disconnectOnMarkerResponseWriter) WriteString(data string) (int, error) {
	return writer.Write([]byte(data))
}

func (writer *disconnectOnMarkerResponseWriter) Flush() {
	if writer.failed {
		writer.flushesAfterError++
	}
	writer.ResponseRecorder.Flush()
}

func anthropicStreamWithMidStreamText() string {
	return strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_disc","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":20,"cache_read_input_tokens":11,"cache_creation_input_tokens":4,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"second"}}`,
		``,
		`event: ping`,
		`data: {"type":"ping"}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
}

// stallingAnthropicUpstream 写完 stream 后保持连接不关闭，模拟上游挂住。
func stallingAnthropicUpstream(t *testing.T, stream string) (*http.Response, *io.PipeWriter) {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, stream)
	}()
	t.Cleanup(func() {
		_ = pw.Close()
		_ = pr.Close()
	})
	return &http.Response{Header: http.Header{}, Body: pr}, pw
}

func newCompatStreamIdleTestService(intervalSec int) *GatewayService {
	return &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamDataIntervalTimeout: intervalSec}}}
}

func TestAnthropicCompatStreaming_IdleUpstreamEndsWithinInterval(t *testing.T) {
	handlers := map[string]func(*GatewayService, *http.Response, *gin.Context) (*ForwardResult, error){
		"chat_completions": func(svc *GatewayService, resp *http.Response, c *gin.Context) (*ForwardResult, error) {
			return svc.handleCCStreamingFromAnthropic(resp, c, "claude-sonnet-4-6", "claude-sonnet-4-6", nil, time.Now())
		},
		"responses": func(svc *GatewayService, resp *http.Response, c *gin.Context) (*ForwardResult, error) {
			return svc.handleResponsesStreamingResponse(resp, c, "claude-sonnet-4-6", "claude-sonnet-4-6", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
		},
	}
	stream := strings.TrimSuffix(anthropicStreamWithMidStreamText(), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n")
	for name, handle := range handlers {
		for _, disconnect := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/client_disconnected=%t", name, disconnect), func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				marker := "\x00never-written"
				if disconnect {
					marker = "second"
				}
				writer := &disconnectOnMarkerResponseWriter{ResponseRecorder: httptest.NewRecorder(), marker: marker}
				c, _ := gin.CreateTestContext(writer)
				resp, pw := stallingAnthropicUpstream(t, stream)

				type outcome struct {
					result *ForwardResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					result, err := handle(newCompatStreamIdleTestService(1), resp, c)
					done <- outcome{result: result, err: err}
				}()
				var got outcome
				select {
				case got = <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("stream did not end after upstream idle interval")
				}

				require.NoError(t, got.err)
				require.Equal(t, disconnect, got.result.ClientDisconnect)
				require.Equal(t, disconnect, writer.failed)
				require.Equal(t, 42, got.result.Usage.OutputTokens)
				require.Zero(t, writer.writesAfterError)
				_, werr := io.WriteString(pw, "event: ping\n")
				require.Error(t, werr, "upstream body must be closed after idle timeout")
			})
		}
	}
}

func TestHandleCCStreamingFromAnthropic_ClientDisconnectDrainsUpstreamUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := &disconnectOnMarkerResponseWriter{ResponseRecorder: httptest.NewRecorder(), marker: "second"}
	c, _ := gin.CreateTestContext(writer)

	result, err := (&GatewayService{}).handleCCStreamingFromAnthropic(
		&http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(anthropicStreamWithMidStreamText()))},
		c, "claude-sonnet-4-6", "claude-sonnet-4-6", nil, time.Now(),
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, writer.failed)
	require.True(t, result.ClientDisconnect)
	require.Zero(t, writer.writesAfterError)
	require.Zero(t, writer.flushesAfterError)

	body := writer.Body.String()
	require.Contains(t, body, `"content":"first"`)
	require.NotContains(t, body, "second")
	require.NotContains(t, body, "[DONE]")

	require.Equal(t, 20, result.Usage.InputTokens)
	require.Equal(t, 42, result.Usage.OutputTokens)
	require.Equal(t, 11, result.Usage.CacheReadInputTokens)
	require.Equal(t, 4, result.Usage.CacheCreationInputTokens)
	require.NotNil(t, result.FirstTokenMs)
}

func TestHandleResponsesStreamingResponse_ClientDisconnectDrainsUpstreamUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := &disconnectOnMarkerResponseWriter{ResponseRecorder: httptest.NewRecorder(), marker: "second"}
	c, _ := gin.CreateTestContext(writer)

	result, err := (&GatewayService{}).handleResponsesStreamingResponse(
		&http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(anthropicStreamWithMidStreamText()))},
		c, "claude-sonnet-4-6", "claude-sonnet-4-6", nil, time.Now(), apicompat.ResponsesClientToolMapping{},
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, writer.failed)
	require.True(t, result.ClientDisconnect)
	require.Zero(t, writer.writesAfterError)
	require.Zero(t, writer.flushesAfterError)

	body := writer.Body.String()
	require.Contains(t, body, `"delta":"first"`)
	require.NotContains(t, body, "second")
	require.NotContains(t, body, "response.completed")

	require.Equal(t, 20, result.Usage.InputTokens)
	require.Equal(t, 42, result.Usage.OutputTokens)
	require.Equal(t, 11, result.Usage.CacheReadInputTokens)
	require.Equal(t, 4, result.Usage.CacheCreationInputTokens)
	require.NotNil(t, result.FirstTokenMs)
}

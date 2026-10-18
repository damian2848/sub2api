package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	geminiWebCannedText    = "I seem to be encountering an error. Can I try something else for you?"
	geminiWebCannedTools   = `"tools":[{"name":"write_file","description":"Write","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]`
	geminiWebToolReplyText = "Writing.\n<tool_call>\n{\"name\": \"write_file\", \"arguments\": {\"path\": \"pelican.html\"}}\n</tool_call>"
)

func geminiWebCannedRequest(endpoint string, stream bool, tools bool) []byte {
	toolField := ""
	if tools {
		toolField = geminiWebCannedTools + ","
	}
	if endpoint == "chat" {
		toolField = ""
		if tools {
			toolField = `"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],`
		}
		return []byte(fmt.Sprintf(`{"model":"gemini-4-argon","stream":%t,%s"messages":[{"role":"user","content":"draw a pelican"}]}`, stream, toolField))
	}
	return []byte(fmt.Sprintf(`{"model":"gemini-4-argon","stream":%t,"max_tokens":64,%s"messages":[{"role":"user","content":"draw a pelican"}]}`, stream, toolField))
}

func forwardGeminiWebCanned(svc *GeminiMessagesCompatService, c *gin.Context, endpoint string, body []byte) (*ForwardResult, error) {
	if endpoint == "chat" {
		return svc.ForwardAsChatCompletions(context.Background(), c, geminiWebTestAccount(), body)
	}
	return svc.Forward(context.Background(), c, geminiWebTestAccount(), body)
}

// With tools, a canned failure reply is held back and becomes a failover error,
// even after first-token pings committed the client response.
func TestGeminiWebCannedReplyFailsOverWithTools(t *testing.T) {
	shortenGeminiWebFirstTokenPing(t)
	for _, endpoint := range []string{"messages", "chat"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", endpoint, stream), func(t *testing.T) {
				frames, _ := geminiWebFramesStub(t, "背景滚动效果。", geminiWebCannedText)
				svc := &GeminiMessagesCompatService{httpUpstream: slowGeminiWebStub(80*time.Millisecond, frames.do)}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
				before := GeminiWebPingAdjustedWrittenSize(c)

				_, err := forwardGeminiWebCanned(svc, c, endpoint, geminiWebCannedRequest(endpoint, stream, true))
				var failover *UpstreamFailoverError
				require.True(t, errors.As(err, &failover), "err = %v", err)
				require.Equal(t, http.StatusBadGateway, failover.StatusCode)
				require.Equal(t, before, GeminiWebPingAdjustedWrittenSize(c), "only pings reached the client")
				out := recorder.Body.String()
				require.NotContains(t, out, "背景滚动效果", "partial answer text was held back")
				require.NotContains(t, out, "encountering an error")
				if stream {
					require.True(t, strings.HasPrefix(out, ": ping\n\n"), "slow stream sent pings first: %q", out)
					require.Empty(t, strings.ReplaceAll(out, ": ping\n\n", ""), "nothing but pings: %q", out)
				}
			})
		}
	}
}

// After a ping-only attempt fails over, the next account writes a normal
// stream on the same committed response.
func TestGeminiWebStreamAfterPingOnlyFailover(t *testing.T) {
	shortenGeminiWebFirstTokenPing(t)
	for _, endpoint := range []string{"messages", "chat"} {
		t.Run(endpoint, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			body := geminiWebCannedRequest(endpoint, true, true)

			canned, _ := geminiWebFramesStub(t, geminiWebCannedText)
			_, err := forwardGeminiWebCanned(&GeminiMessagesCompatService{httpUpstream: slowGeminiWebStub(80*time.Millisecond, canned.do)}, c, endpoint, body)
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover), "err = %v", err)

			good, _ := geminiWebFramesStub(t, "Writing.", geminiWebToolReplyText)
			_, err = forwardGeminiWebCanned(&GeminiMessagesCompatService{httpUpstream: slowGeminiWebStub(80*time.Millisecond, good.do)}, c, endpoint, body)
			require.NoError(t, err)
			out := recorder.Body.String()
			require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
			require.Equal(t, 1, strings.Count(out, "Writing."), "answer text appears once: %q", out)
			require.Contains(t, out, "pelican.html")
			require.NotContains(t, out, "tool_call>")
			if endpoint == "messages" {
				blocks, stop := collectClaudeSSEBlocks(t, out)
				require.Equal(t, "tool_use", stop)
				require.Equal(t, map[string]any{"path": "pelican.html"}, blocks[1]["input"])
			}
		})
	}
}

// Without tools a canned reply fails too. Text that could still become one is
// held back, so a reply that is canned from the start never reaches the client.
func TestGeminiWebCannedReplyWithoutToolsFails(t *testing.T) {
	shortenGeminiWebFirstTokenPing(t)
	for _, endpoint := range []string{"messages", "chat"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", endpoint, stream), func(t *testing.T) {
				frames, _ := geminiWebFramesStub(t, "I seem", "I seem to be encountering", geminiWebCannedText)
				svc := &GeminiMessagesCompatService{httpUpstream: slowGeminiWebStub(80*time.Millisecond, frames.do)}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

				result, err := forwardGeminiWebCanned(svc, c, endpoint, geminiWebCannedRequest(endpoint, stream, false))
				require.Nil(t, result, "a failed reply produces no usage to bill")
				var failover *UpstreamFailoverError
				require.True(t, errors.As(err, &failover), "err = %v", err)
				require.Equal(t, GeminiWebCannedReplyReason, failover.Reason)
				require.Equal(t, GeminiWebCannedReplyClientMessage, failover.ClientMessage)
				require.NotContains(t, recorder.Body.String(), "I seem", "held text never reached the client")
			})
		}
	}
}

// When Gemini Web replaces an answer that has already streamed, the stream
// ends with the canned-reply error instead of a normal finish, and the forward
// fails so nothing is billed.
func TestGeminiWebCannedReplyAfterStreamedTextEndsWithError(t *testing.T) {
	for _, endpoint := range []string{"messages", "chat"} {
		t.Run(endpoint, func(t *testing.T) {
			frames, _ := geminiWebFramesStub(t, "我将创建", "我将创建一个包含鹈鹕的动画。", geminiWebCannedText)
			svc := &GeminiMessagesCompatService{httpUpstream: frames}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

			result, err := forwardGeminiWebCanned(svc, c, endpoint, geminiWebCannedRequest(endpoint, true, false))
			require.Nil(t, result)
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover), "content already streamed: no account switch")
			out := recorder.Body.String()
			require.Contains(t, out, "我将创建")
			require.NotContains(t, out, "encountering an error", "the canned text itself is never sent")
			require.Contains(t, out, "this request was not billed")
		})
	}
}

func TestGeminiWebNativeCannedReplyFails(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			frames, _ := geminiWebFramesStub(t, "I seem to be", geminiWebCannedText)
			svc := &GeminiMessagesCompatService{httpUpstream: frames}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			action := "generateContent"
			if stream {
				action = "streamGenerateContent"
			}
			result, err := svc.ForwardNative(context.Background(), c, geminiWebTestAccount(), "gemini-4-argon", action, stream,
				[]byte(`{"contents":[{"role":"user","parts":[{"text":"draw a pelican"}]}]}`))
			require.Nil(t, result)
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover), "err = %v", err)
			require.Equal(t, GeminiWebCannedReplyReason, failover.Reason)
			require.NotContains(t, recorder.Body.String(), "I seem")
		})
	}
}

func TestGeminiWebCannedReplyErrorIsNotTheCannedText(t *testing.T) {
	require.True(t, errors.Is(geminiweb.ErrCannedReply, geminiweb.ErrCannedReply))
	require.NotContains(t, GeminiWebCannedReplyClientMessage, "encountering")
}

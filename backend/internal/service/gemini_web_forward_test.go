package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/gin-gonic/gin"
)

func TestWebUsageEstimatesPlainTextAndThinking(t *testing.T) {
	withoutThinking := webUsageWithThinking("hello world", "answer", "")
	usage := webUsageWithThinking("hello world", "answer", "hidden reasoning")
	if usage.InputTokens <= 0 || usage.OutputTokens <= 0 {
		t.Fatalf("usage = %#v, want positive text estimates", usage)
	}
	if usage.OutputTokens <= withoutThinking.OutputTokens {
		t.Fatalf("thinking text was not included: %#v", usage)
	}
}

func TestGeminiWebNativeStreamWriterAbortBeforeCommitAllowsFailover(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	w := newGeminiWebNativeStreamWriter(c, "gemini-pro", nil)
	err := w.Abort(geminiweb.ErrRateLimited)
	var failover *UpstreamFailoverError
	if !errors.As(err, &failover) || failover.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Abort() = %v, want rate-limit failover", err)
	}
	if c.Writer.Written() || recorder.Body.Len() != 0 {
		t.Fatal("failed uncommitted attempt wrote a response")
	}
}

// Heartbeats carry no content: an attempt that only sent them (or found
// earlier output on the response) still switches accounts on the same stream.
func TestGeminiWebNativeStreamWriterHeartbeatOnlyAbortFailsOver(t *testing.T) {
	for _, commitMode := range []string{"heartbeat", "earlier-concurrency-heartbeat"} {
		t.Run(commitMode, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			if commitMode == "earlier-concurrency-heartbeat" {
				_, _ = c.Writer.Write([]byte("\n\n"))
			}
			w := newGeminiWebNativeStreamWriter(c, "gemini-pro", nil)
			w.mu.Lock()
			_ = w.writeHeartbeatLocked()
			w.mu.Unlock()
			err := w.Abort(geminiweb.ErrCannedReply)
			var failover *UpstreamFailoverError
			if !errors.As(err, &failover) || failover.Reason != GeminiWebCannedReplyReason {
				t.Fatalf("heartbeat-only Abort() = %v, want canned-reply failover", err)
			}
			if body := strings.ReplaceAll(recorder.Body.String(), "\n", ""); body != "" {
				t.Fatalf("only heartbeats may precede a failover, got %q", recorder.Body.String())
			}
		})
	}
}

func TestGeminiWebNativeStreamWriterCommittedAbortEmitsGoogleError(t *testing.T) {
	for _, commitMode := range []string{"content"} {
		t.Run(commitMode, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			if commitMode == "earlier-concurrency-heartbeat" {
				_, _ = c.Writer.Write([]byte("\n\n"))
			}
			w := newGeminiWebNativeStreamWriter(c, "gemini-pro", nil)
			switch commitMode {
			case "heartbeat":
				w.mu.Lock()
				_ = w.writeHeartbeatLocked()
				w.mu.Unlock()
			case "content":
				if err := w.WriteResult(geminiweb.Result{Text: "partial answer"}); err != nil {
					t.Fatal(err)
				}
			}
			err := w.Abort(geminiweb.ErrInvalidCredential)
			var failover *UpstreamFailoverError
			if err == nil || errors.As(err, &failover) {
				t.Fatalf("committed Abort() = %v, must not allow failover", err)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("committed stream HTTP status = %d", recorder.Code)
			}
			var last map[string]json.RawMessage
			for _, event := range strings.Split(recorder.Body.String(), "\n\n") {
				if event == "" {
					continue
				}
				if !strings.HasPrefix(event, "data: ") {
					t.Fatalf("non-SSE content in response: %q", event)
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(event, "data: ")), &last); err != nil {
					t.Fatal(err)
				}
			}
			var googleError struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			}
			if err := json.Unmarshal(last["error"], &googleError); err != nil {
				t.Fatalf("missing Google error frame: %s", recorder.Body.String())
			}
			if googleError.Code != http.StatusUnauthorized || googleError.Status != "UNAUTHENTICATED" || googleError.Message != GeminiWebCredentialRejectedClientMessage {
				t.Fatalf("Google error = %#v", googleError)
			}
			if streamErr, ok := GetOpsStreamError(c); !ok || streamErr.IntendedStatus != http.StatusUnauthorized || !streamErr.CountTowardsSLA {
				t.Fatalf("stream failure was not recorded: %#v", streamErr)
			}
		})
	}
}

func TestGeminiWebNativeStreamWriterAbortSanitizesUpstreamError(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	w := newGeminiWebNativeStreamWriter(c, "gemini-pro", nil)
	if err := w.WriteResult(geminiweb.Result{Text: "partial"}); err != nil {
		t.Fatal(err)
	}
	err := w.Abort(errors.New("upstream detail containing secret-cookie"))
	if err == nil || strings.Contains(err.Error(), "secret-cookie") || strings.Contains(recorder.Body.String(), "secret-cookie") {
		t.Fatalf("upstream detail leaked: err=%v body=%q", err, recorder.Body.String())
	}
}

type geminiWebFailedResponseWriter struct {
	gin.ResponseWriter
	err    error
	writes int
}

func (w *geminiWebFailedResponseWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, w.err
}

func TestGeminiWebNativeStreamWriterWriteFailureCancelsUpstream(t *testing.T) {
	for _, failureAt := range []string{"heartbeat", "content", "finish"} {
		t.Run(failureAt, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			writeErr := errors.New("downstream disconnected")
			failed := &geminiWebFailedResponseWriter{ResponseWriter: c.Writer, err: writeErr}
			c.Writer = failed
			upstreamCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := newGeminiWebNativeStreamWriter(c, "gemini-pro", cancel)
			var err error
			switch failureAt {
			case "heartbeat":
				w.mu.Lock()
				err = w.writeHeartbeatLocked()
				w.mu.Unlock()
			case "content":
				err = w.WriteResult(geminiweb.Result{Text: "hello"})
			case "finish":
				err = w.Finish(geminiweb.Result{}, ClaudeUsage{InputTokens: 2, OutputTokens: 1})
			}
			if !errors.Is(err, writeErr) || !errors.Is(upstreamCtx.Err(), context.Canceled) {
				t.Fatalf("write error = %v, upstream context = %v", err, upstreamCtx.Err())
			}
			err = w.Abort(context.Canceled)
			var failover *UpstreamFailoverError
			if !errors.Is(err, writeErr) || errors.As(err, &failover) || failed.writes != 1 {
				t.Fatalf("Abort() = %v, writes = %d", err, failed.writes)
			}
			if err := w.Finish(geminiweb.Result{Text: "late result"}, ClaudeUsage{}); !errors.Is(err, writeErr) {
				t.Fatalf("Finish() lost downstream failure: %v", err)
			}
		})
	}
}

func TestGeminiWebNativeStreamWriterCanceledRequestDoesNotFailover(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	w := newGeminiWebNativeStreamWriter(c, "gemini-pro", nil)
	cancel()
	err := w.Abort(context.Canceled)
	if !errors.Is(err, context.Canceled) || c.Writer.Written() {
		t.Fatalf("canceled Abort() = %v, written=%v", err, c.Writer.Written())
	}
}

func TestGeminiWebNativeStreamWriterFinishEmitsDeltasAndUsage(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	w := newGeminiWebNativeStreamWriter(c, "gemini-pro", nil)
	if err := w.WriteResult(geminiweb.Result{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(geminiweb.Result{Text: "hello world"}, ClaudeUsage{InputTokens: 2, OutputTokens: 3}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if strings.Count(body, "data: ") != 3 || !strings.Contains(body, `"text":"hello"`) || !strings.Contains(body, `"text":" world"`) || !strings.Contains(body, `"finishReason":"STOP"`) || !strings.Contains(body, `"totalTokenCount":5`) {
		t.Fatalf("unexpected stream: %q", body)
	}
	if err := w.WriteResult(geminiweb.Result{Text: "late callback"}); err == nil || recorder.Body.String() != body {
		t.Fatal("callback after terminal frame appended content")
	}
}

func TestGeminiWebNativeStreamWriterRecordsFirstTokenMs(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	w := newGeminiWebNativeStreamWriter(c, "gemini-pro", nil)
	w.start = time.Now().Add(-50 * time.Millisecond)
	w.mu.Lock()
	_ = w.writeHeartbeatLocked()
	w.mu.Unlock()
	if w.firstTokenMs != nil {
		t.Fatal("a heartbeat is not a first token")
	}
	if err := w.WriteResult(geminiweb.Result{Thinking: "planning"}); err != nil {
		t.Fatal(err)
	}
	if w.firstTokenMs == nil || *w.firstTokenMs < 50 {
		t.Fatalf("firstTokenMs = %v, want >= 50", w.firstTokenMs)
	}
	first := *w.firstTokenMs
	if err := w.Finish(geminiweb.Result{Thinking: "planning", Text: "answer"}, ClaudeUsage{}); err != nil {
		t.Fatal(err)
	}
	if *w.firstTokenMs != first {
		t.Fatal("later frames overwrote the first-token time")
	}
}

func TestGeminiWebNativeStreamWriterUsesBlankSSEHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	writer := &geminiWebNativeStreamWriter{c: ctx}

	writer.mu.Lock()
	err := writer.writeHeartbeatLocked()
	writer.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	if got := recorder.Body.String(); got != "\n\n" {
		t.Fatalf("heartbeat = %q, want %q", got, "\n\n")
	}
}

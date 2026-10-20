package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// geminiWebFlaggedFrame is one StreamGenerate frame; failed sets payload[31],
// Google's marker for a failed generation.
type geminiWebFlaggedFrame struct {
	text   string
	failed bool
}

func geminiWebFlaggedFrames(t *testing.T, frames ...geminiWebFlaggedFrame) string {
	t.Helper()
	body := ""
	for _, f := range frames {
		payload := make([]any, 43)
		payload[4] = []any{[]any{"candidate", []any{f.text}}}
		payload[39] = "cf41b0e0dd7d53e5"
		if f.failed {
			payload[31] = true
		}
		b, err := json.Marshal(payload)
		require.NoError(t, err)
		outer, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(b)}})
		require.NoError(t, err)
		body += string(outer) + "\n"
	}
	return body
}

// geminiWebAttemptsStub answers the n-th StreamGenerate call with
// attempts[n], after delay.
func geminiWebAttemptsStub(t *testing.T, delay time.Duration, attempts ...[]geminiWebFlaggedFrame) (*geminiWebUsageHTTPStub, *int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	return &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
		body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
		if strings.Contains(req.URL.Path, "StreamGenerate") {
			mu.Lock()
			n := calls
			calls++
			mu.Unlock()
			time.Sleep(delay)
			body = geminiWebFlaggedFrames(t, attempts[min(n, len(attempts)-1)]...)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}}, &calls
}

func shortenGeminiWebNativeHeartbeat(t *testing.T) {
	t.Helper()
	previous := geminiWebNativeHeartbeatInterval
	geminiWebNativeHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { geminiWebNativeHeartbeatInterval = previous })
}

func geminiWebTestContext() (*httptest.ResponseRecorder, *gin.Context) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return recorder, c
}

// Google's failed-generation flag fails the reply even when its wording is
// not in the phrase catalog.
func TestGeminiWebGenerationFailedFlagFailsUnknownWording(t *testing.T) {
	const wording = "Something unexpected happened on our side."
	for _, endpoint := range []string{"native", "messages", "chat"} {
		t.Run(endpoint, func(t *testing.T) {
			upstream, _ := geminiWebAttemptsStub(t, 0, []geminiWebFlaggedFrame{{text: wording, failed: true}})
			svc := &GeminiMessagesCompatService{httpUpstream: upstream}
			recorder, c := geminiWebTestContext()
			var result *ForwardResult
			var err error
			switch endpoint {
			case "native":
				result, err = svc.ForwardNative(context.Background(), c, geminiWebTestAccount(), "gemini-4-argon", "generateContent", false,
					[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
			case "messages":
				result, err = svc.Forward(context.Background(), c, geminiWebTestAccount(), geminiWebCannedRequest("messages", false, false))
			case "chat":
				result, err = svc.ForwardAsChatCompletions(context.Background(), c, geminiWebTestAccount(), geminiWebCannedRequest("chat", false, false))
			}
			require.Nil(t, result, "a failed generation produces no usage to bill")
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover), "err = %v", err)
			require.Equal(t, GeminiWebCannedReplyReason, failover.Reason)
			require.NotContains(t, recorder.Body.String(), wording)
		})
	}
}

// A real answer is never failed by the flag check.
func TestGeminiWebUnflaggedReplyPasses(t *testing.T) {
	upstream, _ := geminiWebAttemptsStub(t, 0, []geminiWebFlaggedFrame{{text: "Something unexpected happened in the story."}})
	svc := &GeminiMessagesCompatService{httpUpstream: upstream}
	recorder, c := geminiWebTestContext()
	_, err := svc.ForwardNative(context.Background(), c, geminiWebTestAccount(), "gemini-4-argon", "generateContent", false,
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	require.NoError(t, err)
	require.Contains(t, recorder.Body.String(), "Something unexpected happened in the story.")
}

// A native stream that has only sent heartbeats switches accounts on a
// canned reply, and the next account's answer streams on the same response.
func TestGeminiWebNativeStreamFailsOverAfterHeartbeats(t *testing.T) {
	shortenGeminiWebNativeHeartbeat(t)
	for _, tools := range []bool{false, true} {
		t.Run(fmt.Sprintf("tools=%t", tools), func(t *testing.T) {
			body := `{"contents":[{"role":"user","parts":[{"text":"draw a pelican"}]}]}`
			canned := []geminiWebFlaggedFrame{{text: "I encountered an error doing what you asked. Could you try again?", failed: true}}
			answer := []geminiWebFlaggedFrame{{text: "Here is"}, {text: "Here is the pelican."}}
			if tools {
				body = `{"tools":[{"functionDeclarations":[{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}],"contents":[{"role":"user","parts":[{"text":"draw a pelican"}]}]}`
				// With tools the answer text is held, so even a canned reply
				// that follows streamed text can still fail over.
				canned = []geminiWebFlaggedFrame{{text: "Writing the file."}, {text: "I encountered an error doing what you asked. Could you try again?", failed: true}}
				answer = []geminiWebFlaggedFrame{{text: "Writing the file."}, {text: "Writing the file.\n<tool_call>\n{\"name\": \"write_file\", \"arguments\": {\"path\": \"pelican.html\"}}\n</tool_call>"}}
			}
			upstream, _ := geminiWebAttemptsStub(t, 80*time.Millisecond, canned, answer)
			svc := &GeminiMessagesCompatService{httpUpstream: upstream}
			recorder, c := geminiWebTestContext()

			_, err := svc.ForwardNative(context.Background(), c, geminiWebTestAccount(), "gemini-4-argon", "streamGenerateContent", true, []byte(body))
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover), "first attempt err = %v", err)
			require.Equal(t, GeminiWebCannedReplyReason, failover.Reason)
			require.True(t, c.Writer.Written(), "heartbeats committed the stream")
			require.Empty(t, strings.TrimSpace(recorder.Body.String()), "only heartbeats before the failover: %q", recorder.Body.String())

			result, err := svc.ForwardNative(context.Background(), c, geminiWebTestAccount(), "gemini-4-argon", "streamGenerateContent", true, []byte(body))
			require.NoError(t, err, "the next account answers on the same response")
			require.NotNil(t, result)
			out := recorder.Body.String()
			require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
			require.NotContains(t, out, "encountered an error")
			require.NotContains(t, out, `"error"`)
			if tools {
				require.Equal(t, 1, strings.Count(out, "Writing the file."), "held text is sent once: %q", out)
				require.Contains(t, out, `"functionCall":{"args":{"path":"pelican.html"},"name":"write_file"}`)
			} else {
				require.Contains(t, out, "the pelican.")
			}
		})
	}
}

// A streamed snapshot carrying the failed-generation flag is held, so its
// text never reaches the client.
func TestGeminiWebFlaggedSnapshotIsHeld(t *testing.T) {
	for _, endpoint := range []string{"native", "messages", "chat"} {
		t.Run(endpoint, func(t *testing.T) {
			upstream, _ := geminiWebAttemptsStub(t, 0, []geminiWebFlaggedFrame{{text: "Oops, that did not work.", failed: true}})
			svc := &GeminiMessagesCompatService{httpUpstream: upstream}
			recorder, c := geminiWebTestContext()
			var err error
			switch endpoint {
			case "native":
				_, err = svc.ForwardNative(context.Background(), c, geminiWebTestAccount(), "gemini-4-argon", "streamGenerateContent", true,
					[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
			case "messages":
				_, err = svc.Forward(context.Background(), c, geminiWebTestAccount(), geminiWebCannedRequest("messages", true, false))
			case "chat":
				_, err = svc.ForwardAsChatCompletions(context.Background(), c, geminiWebTestAccount(), geminiWebCannedRequest("chat", true, false))
			}
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover), "err = %v", err)
			require.NotContains(t, recorder.Body.String(), "Oops")
		})
	}
}

// geminiWebCooldownRepo records temporary unschedules.
type geminiWebCooldownRepo struct {
	AccountRepository
	mu    sync.Mutex
	calls []time.Time
}

func (r *geminiWebCooldownRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if reason != geminiWebCannedCooldownReason {
		return fmt.Errorf("unexpected reason %q", reason)
	}
	r.calls = append(r.calls, until)
	return nil
}

func TestGeminiWebCannedTrackerThreshold(t *testing.T) {
	var tracker geminiWebCannedTracker
	start := time.Now()
	require.False(t, tracker.record(1, start))
	require.False(t, tracker.record(1, start.Add(time.Minute)))
	require.False(t, tracker.record(2, start.Add(time.Minute)), "accounts are counted separately")
	require.True(t, tracker.record(1, start.Add(2*time.Minute)), "third reply within the window")
	require.False(t, tracker.record(1, start.Add(3*time.Minute)), "reaching the threshold resets the count")

	var spread geminiWebCannedTracker
	require.False(t, spread.record(1, start))
	require.False(t, spread.record(1, start.Add(6*time.Minute)))
	require.False(t, spread.record(1, start.Add(12*time.Minute)), "replies outside the window do not add up")
}

// Repeated canned replies take the account out of scheduling for a while.
func TestGeminiWebRepeatedCannedRepliesCoolDownAccount(t *testing.T) {
	upstream, _ := geminiWebAttemptsStub(t, 0, []geminiWebFlaggedFrame{{text: geminiWebCannedText, failed: true}})
	repo := &geminiWebCooldownRepo{}
	svc := &GeminiMessagesCompatService{httpUpstream: upstream, accountRepo: repo}
	account := geminiWebTestAccount()
	account.ID = 245
	for i := 1; i <= geminiWebCannedCooldownThreshold; i++ {
		_, c := geminiWebTestContext()
		_, err := svc.ForwardNative(context.Background(), c, account, "gemini-4-argon", "generateContent", false,
			[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
		require.Error(t, err)
		if i < geminiWebCannedCooldownThreshold {
			require.Empty(t, repo.calls, "no cooldown after %d canned replies", i)
		}
	}
	require.Len(t, repo.calls, 1)
	require.WithinDuration(t, time.Now().Add(geminiWebCannedCooldownDuration), repo.calls[0], 5*time.Second)
	require.NotNil(t, account.TempUnschedulableUntil)
}

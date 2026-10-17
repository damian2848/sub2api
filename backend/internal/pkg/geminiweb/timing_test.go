package geminiweb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

const timingStep = 30 * time.Millisecond

func timingSessionDo(post func(*http.Request) (*http.Response, error)) Doer {
	return func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
		}
		return post(req)
	}
}

// thinkingFrameBody is a frame that carries only a thought, as Gemini Web
// sends while a thinking model has not started its answer.
func thinkingFrameBody(thought string) string {
	candidate := make([]any, 38)
	candidate[37] = []any{[]any{thought}}
	payload, _ := json.Marshal([]any{nil, []any{"cid", "rid"}, nil, nil, []any{candidate}})
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", "req", string(payload)}})
	return ")]}'\n" + string(outer)
}

func TestCompleteRecordsStageTimings(t *testing.T) {
	do := timingSessionDo(func(*http.Request) (*http.Response, error) {
		time.Sleep(timingStep)
		reader, writer := io.Pipe()
		go func() {
			time.Sleep(timingStep)
			_, _ = io.WriteString(writer, thinkingFrameBody("planning"))
			time.Sleep(timingStep)
			_, _ = io.WriteString(writer, "\n"+frameBody("hello"))
			_ = writer.Close()
		}()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader}, nil
	})

	result, err := NewClient().Complete(context.Background(), Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"},
		Request{Model: "gemini-3.8-flash", Prompt: "你好abc"}, do)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	got := result.Timing
	if got.PromptChars != 5 {
		t.Errorf("PromptChars = %d, want 5 runes", got.PromptChars)
	}
	if got.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", got.Status)
	}
	if got.Session <= 0 || got.Session > got.Headers {
		t.Errorf("Session = %v, want reached before Headers %v", got.Session, got.Headers)
	}
	if got.Headers < timingStep {
		t.Errorf("Headers = %v, want at least the %v header delay", got.Headers, timingStep)
	}
	if got.FirstFrame-got.Headers < timingStep*2/3 {
		t.Errorf("FirstFrame = %v, want about %v after Headers %v", got.FirstFrame, timingStep, got.Headers)
	}
	if got.FirstThinking < got.FirstFrame || got.FirstThinking-got.FirstFrame > timingStep/3 {
		t.Errorf("FirstThinking = %v, want the first frame %v", got.FirstThinking, got.FirstFrame)
	}
	if got.FirstText-got.FirstThinking < timingStep*2/3 {
		t.Errorf("FirstText = %v, want about %v after FirstThinking %v", got.FirstText, timingStep, got.FirstThinking)
	}
	if got.Total < got.FirstText {
		t.Errorf("Total = %v, want at least FirstText %v", got.Total, got.FirstText)
	}
}

func TestCompleteTimingWhenCanceledBeforeFirstFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	go func() {
		<-ctx.Done()
		_ = writer.CloseWithError(ctx.Err())
	}()
	do := timingSessionDo(func(*http.Request) (*http.Response, error) {
		time.AfterFunc(timingStep, cancel)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader}, nil
	})

	result, err := NewClient().Complete(ctx, Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"},
		Request{Model: "gemini-4-argon", Prompt: "hello"}, do)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete() error = %v, want context.Canceled", err)
	}
	got := result.Timing
	if got.Headers <= 0 || got.Status != http.StatusOK {
		t.Errorf("Headers = %v, Status = %d, want headers recorded", got.Headers, got.Status)
	}
	if got.FirstFrame != 0 || got.FirstThinking != 0 || got.FirstText != 0 {
		t.Errorf("frame stages = %v/%v/%v, want all unreached", got.FirstFrame, got.FirstThinking, got.FirstText)
	}
	if got.Total < got.Headers+timingStep*2/3 {
		t.Errorf("Total = %v, want about %v after Headers %v", got.Total, timingStep, got.Headers)
	}
}

func TestCompleteTimingWhenTransportFails(t *testing.T) {
	do := timingSessionDo(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})

	result, err := NewClient().Complete(context.Background(), Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"},
		Request{Model: "gemini-3.8-flash", Prompt: "hello"}, do)
	if err == nil {
		t.Fatal("Complete() error = nil, want transport error")
	}
	got := result.Timing
	if got.Session <= 0 || got.Total <= 0 || got.PromptChars != 5 {
		t.Errorf("Session = %v, Total = %v, PromptChars = %d, want session, total and prompt size recorded", got.Session, got.Total, got.PromptChars)
	}
	if got.Headers != 0 || got.Status != 0 {
		t.Errorf("Headers = %v, Status = %d, want unreached", got.Headers, got.Status)
	}
}

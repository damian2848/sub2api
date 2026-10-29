package geminiweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestCompleteRetainsUpstreamHTTPStatus(t *testing.T) {
	for _, stage := range []string{"session", "generation"} {
		for _, status := range []int{500, 502, 503, 504, 418} {
			t.Run(fmt.Sprintf("%s/%d", stage, status), func(t *testing.T) {
				do := func(req *http.Request) (*http.Response, error) {
					if stage == "generation" && req.Method == http.MethodGet {
						return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
					}
					return response(status, "private upstream details", nil), nil
				}
				_, err := NewClient().Complete(context.Background(),
					Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"},
					Request{Model: "gemini-4-argon", Prompt: "hello"}, do)
				var upstream *UpstreamHTTPError
				if !errors.As(err, &upstream) || upstream.StatusCode != status {
					t.Fatalf("error = %v, want typed HTTP %d", err, status)
				}
				if strings.Contains(err.Error(), "private upstream details") {
					t.Fatalf("error includes upstream body: %v", err)
				}
			})
		}
	}
}

// This is the HTTP-200 failure envelope observed from StreamGenerate. It has
// no answer payload; the error is in wrb.fr[5], followed by transport frames.
const bard1095Frame = `[["wrb.fr",null,null,null,null,[8,null,[["type.googleapis.com/assistant.boq.bard.application.BardErrorInfo",[1095]]]]]]`

func TestCompleteRetainsStructuredRPCError(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		rpc  int
		bard int
		want error
	}{
		{name: "observed 1095", raw: bard1095Frame, rpc: 8, bard: 1095},
		{name: "legacy Bard type", raw: strings.Replace(bard1095Frame, "assistant.boq.bard.application", "google.internal.bard.v1", 1), rpc: 8, bard: 1095},
		{name: "unrecognized Bard type namespace", raw: strings.Replace(bard1095Frame, "assistant.boq.bard.application", "unrecognized.namespace", 1), rpc: 8},
		{name: "error remains terminal", raw: bard1095Frame + "\n" + frameBody("ignored after upstream failure"), rpc: 8, bard: 1095},
		{name: "credential", raw: strings.Replace(bard1095Frame, "1095", "1016", 1), rpc: 8, bard: 1016, want: ErrInvalidCredential},
		{name: "rate limit 1037", raw: strings.Replace(bard1095Frame, "1095", "1037", 1), rpc: 8, bard: 1037, want: ErrRateLimited},
		{name: "rate limit 1013", raw: strings.Replace(bard1095Frame, "1095", "1013", 1), rpc: 8, bard: 1013, want: ErrRateLimited},
		{name: "known 1050", raw: strings.Replace(bard1095Frame, "1095", "1050", 1), rpc: 8, bard: 1050},
		{name: "known 1052", raw: strings.Replace(bard1095Frame, "1095", "1052", 1), rpc: 8, bard: 1052},
		{name: "known 1060", raw: strings.Replace(bard1095Frame, "1095", "1060", 1), rpc: 8, bard: 1060},
		{name: "unknown Bard code", raw: strings.Replace(bard1095Frame, "1095", "123456", 1), rpc: 8, bard: 123456},
		{name: "RPC only", raw: `[["wrb.fr",null,null,null,null,[13,"private upstream details",null]]]`, rpc: 13},
		{name: "message is not a Bard code", raw: `[["wrb.fr",null,null,null,null,[8,"private upstream details: BardErrorInfo 1016",null]]]`, rpc: 8},
		{name: "unrelated detail type", raw: `[["wrb.fr",null,null,null,null,[8,null,[["private upstream details/BardErrorInfo",[1016]]]]]]`, rpc: 8},
		{name: "unrelated detail field", raw: `[["wrb.fr",null,null,null,null,[8,null,[["type.googleapis.com/assistant.boq.bard.application.BardErrorInfo",[1095,1016,1037]]]]]]`, rpc: 8, bard: 1095},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := ")]}'\n\n" + fmt.Sprint(len(tc.raw)) + "\n" + tc.raw + "\n[[\"di\",8],[\"af.httprm\",8,\"1016\"]]"
			do := func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
				}
				return response(http.StatusOK, body, nil), nil
			}
			result, err := NewClient().Complete(context.Background(),
				Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"},
				Request{Model: "gemini-4-argon", Prompt: "hi", OnResult: func(Result) error {
					t.Error("RPC failure must not emit an answer snapshot")
					return nil
				}}, do)
			var upstream *UpstreamRPCError
			if !errors.As(err, &upstream) || upstream.RPCCode != tc.rpc || upstream.BardCode != tc.bard {
				t.Fatalf("error = %v, want RPC %d / Bard %d", err, tc.rpc, tc.bard)
			}
			for _, sentinel := range []error{ErrInvalidCredential, ErrRateLimited, ErrEmptyResponse} {
				if errors.Is(err, sentinel) != (sentinel == tc.want) {
					t.Errorf("error = %v, unexpected classification for %v", err, sentinel)
				}
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("RPC error %d", tc.rpc)) ||
				(tc.bard != 0 && !strings.Contains(err.Error(), fmt.Sprintf("BardErrorInfo %d", tc.bard))) {
				t.Errorf("error does not identify upstream codes: %v", err)
			}
			if strings.Contains(err.Error(), "private upstream details") {
				t.Errorf("error leaks upstream message: %v", err)
			}
			if string(result.Raw) != body || result.Timing.Status != http.StatusOK {
				t.Error("RPC error did not retain raw diagnostic body and HTTP status")
			}
		})
	}
}

func TestCompleteDoesNotClassifyAnswerTextAsRPCError(t *testing.T) {
	answer := "BardErrorInfo codes: 1016, 1037, 1050, 1052, 1060, 1013, 1095. Example: " + bard1095Frame
	for _, status := range []json.RawMessage{nil, json.RawMessage(`[0,null,null]`)} {
		t.Run(string(status), func(t *testing.T) {
			payload, _ := json.Marshal([]any{nil, nil, nil, nil, []any{[]any{nil, []any{answer}}}})
			envelope, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(payload), nil, nil, status}})
			do := func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
				}
				return response(http.StatusOK, string(envelope), nil), nil
			}
			result, err := NewClient().Complete(context.Background(),
				Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"},
				Request{Model: "gemini-4-argon", Prompt: "explain these errors"}, do)
			if err != nil || result.Text != answer {
				t.Fatalf("quoted protocol data must remain answer text: text=%q err=%v", result.Text, err)
			}
		})
	}
}

func TestFrameParserKeepsRPCErrorAcrossEveryChunkBoundary(t *testing.T) {
	body := ")]}'\n" + fmt.Sprint(len(bard1095Frame)) + "\n" + bard1095Frame + "\n[[\"di\",8],[\"af.httprm\",8,\"1016\"]]"
	for i := 1; i < len(body); i++ {
		p := newFrameParser()
		frames := append(p.feed(body[:i]), p.feed(body[i:])...)
		frames = append(frames, p.flush()...)
		if len(frames) != 1 || frames[0].err == nil || frames[0].err.RPCCode != 8 || frames[0].err.BardCode != 1095 {
			t.Fatalf("split at byte %d decoded %#v", i, frames)
		}
	}
}

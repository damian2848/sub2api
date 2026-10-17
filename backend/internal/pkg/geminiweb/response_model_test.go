package geminiweb

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestClientCompleteObservesResponseModel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ids      []any
		want     string
		conflict bool
	}{
		{"downgraded", []any{nil, "cf41b0e0dd7d53e5", nil}, "gemini-3.5-flash-lite", false},
		{"matching", []any{"95a4c0abcba24364", "95a4c0abcba24364"}, "gemini-4-argon", false},
		{"missing", []any{nil, " ", 42}, "", false},
		{"unknown", []any{"unknown-runtime-id"}, "unknown-runtime-id", false},
		{"conflicting", []any{"95a4c0abcba24364", "cf41b0e0dd7d53e5", nil}, "gemini-3.5-flash-lite", true},
		{"bounded", []any{strings.Repeat("x", 300)}, strings.Repeat("x", 200), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var frames strings.Builder
			for _, id := range tc.ids {
				payload := make([]any, 43)
				// Model IDs in generated text or a display label alone are not
				// declarations of the model that served the response.
				payload[4] = []any{[]any{"candidate", []any{"95a4c0abcba24364 cf41b0e0dd7d53e5"}}}
				payload[39], payload[42] = id, "Gemini 4 Argon"
				b, _ := json.Marshal(payload)
				outer, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(b)}})
				_, _ = frames.Write(outer)
				_, _ = frames.WriteString("\n")
			}
			do := func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
				}
				if !strings.Contains(req.Header.Get("x-goog-ext-525001261-jspb"), "95a4c0abcba24364") {
					t.Fatal("request did not select Argon")
				}
				return response(http.StatusOK, frames.String(), nil), nil
			}
			var last Result
			result, err := NewClient().Complete(context.Background(), Credential{Cookie: "__Secure-1PSID=psid"}, Request{
				Model: "gemini-4-argon", Prompt: "hello",
				OnResult: func(r Result) error { last = r; return nil },
			}, do)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range []Result{result, last} {
				if r.ResponseModel != tc.want || r.ResponseModelConflict != tc.conflict {
					t.Fatalf("model=%q conflict=%v, want %q/%v", r.ResponseModel, r.ResponseModelConflict, tc.want, tc.conflict)
				}
			}
		})
	}
}

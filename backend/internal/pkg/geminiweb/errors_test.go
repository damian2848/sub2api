package geminiweb

import (
	"context"
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

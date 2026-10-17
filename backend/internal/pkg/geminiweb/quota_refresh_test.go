package geminiweb

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestClientRefreshQuotaPreservesSessionAndLiveCookies(t *testing.T) {
	client := NewClient()
	cred := Credential{Cookie: "__Secure-1PSID=psid; __Secure-1PSIDTS=original", BaseURL: "https://gemini.test"}
	bootstraps, queries := 0, 0
	do := func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			bootstraps++
			return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
		}
		queries++
		if req.URL.Query().Get("rpcids") != geminiWebQuotaRPC {
			t.Fatalf("unexpected request: %s", req.URL)
		}
		fraction, rotated := "0.25", "renewed"
		if queries == 2 {
			if !strings.Contains(req.Header.Get("Cookie"), "__Secure-1PSIDTS=renewed") {
				t.Fatal("forced quota refresh did not use the live rotated cookie")
			}
			fraction, rotated = "0.6", "latest"
		}
		payload, _ := json.Marshal([]any{2, []any{[]any{"", fraction, 1, []any{"1730000000", "0"}}}})
		outer, _ := json.Marshal([]any{[]any{"wrb.fr", geminiWebQuotaRPC, string(payload)}})
		return response(http.StatusOK, ")]}'\n"+string(outer), http.Header{
			"Set-Cookie": {"__Secure-1PSIDTS=" + rotated + "; Domain=.google.com; Path=/; Secure"},
		}), nil
	}
	initial, err := client.FetchQuota(context.Background(), cred, do)
	if err != nil || len(initial.Quota.Windows) != 1 || initial.Quota.Windows[0].UsedPercent != 25 {
		t.Fatalf("initial quota = %#v, error = %v", initial, err)
	}
	if _, err := client.FetchQuota(context.Background(), cred, do); err != nil || queries != 1 {
		t.Fatalf("ordinary query did not reuse quota cache: queries=%d error=%v", queries, err)
	}
	refreshed, err := client.RefreshQuota(context.Background(), cred, do)
	if err != nil || len(refreshed.Quota.Windows) != 1 || refreshed.Quota.Windows[0].UsedPercent != 60 {
		t.Fatalf("refreshed quota = %#v, error = %v", refreshed, err)
	}
	if bootstraps != 1 || queries != 2 {
		t.Fatalf("forced refresh requests: bootstraps=%d queries=%d, want 1 and 2", bootstraps, queries)
	}
	if !strings.Contains(refreshed.UpdatedCookie, "__Secure-1PSIDTS=latest") || client.CurrentCookie(cred) != refreshed.UpdatedCookie {
		t.Fatal("forced refresh did not preserve the latest rotated cookie")
	}
	cached, err := client.FetchQuota(context.Background(), cred, do)
	if err != nil || len(cached.Quota.Windows) != 1 || cached.Quota.Windows[0].UsedPercent != 60 || queries != 2 {
		t.Fatalf("refreshed quota was not cached: quota=%#v queries=%d error=%v", cached, queries, err)
	}
}

package geminiweb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestClientCompleteDiscoversSessionAndParsesAnswer(t *testing.T) {
	cookie := "Cookie: __Secure-1PSID=psid; __Secure-1PSIDTS=ts"
	var calls int
	do := func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method == http.MethodGet {
			if req.Header.Get("Cookie") != "__Secure-1PSID=psid; __Secure-1PSIDTS=ts" {
				t.Fatalf("normalized cookie = %q", req.Header.Get("Cookie"))
			}
			return response(http.StatusOK, `<html>"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid" "TuX5cc":"en"</html>`, nil), nil
		}
		if req.Header.Get("Cookie") == "" || !strings.Contains(req.Header.Get("x-goog-ext-525001261-jspb"), "56fdd199312815e2") {
			t.Fatalf("missing browser request headers: cookie=%q model=%q", req.Header.Get("Cookie"), req.Header.Get("x-goog-ext-525001261-jspb"))
		}
		body, _ := io.ReadAll(req.Body)
		values, _ := url.ParseQuery(string(body))
		if values.Get("at") != "token" || values.Get("f.req") == "" {
			t.Fatalf("unexpected form body: %s", body)
		}
		return response(http.StatusOK, frameBody("hello from web"), nil), nil
	}

	result, err := NewClient().Complete(context.Background(), Credential{Cookie: cookie, BaseURL: "https://gemini.test"}, Request{Model: "gemini-3.8-flash", Prompt: "hello"}, do)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if result.Text != "hello from web" || result.CID != "cid" || result.RID != "rid" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if calls != 2 {
		t.Fatalf("request count = %d, want session + completion", calls)
	}
}

func TestClientCompleteRefreshesRejectedSession(t *testing.T) {
	posts := 0
	gets := 0
	do := func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			gets++
			return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
		}
		posts++
		if posts == 1 {
			return response(http.StatusMethodNotAllowed, "", nil), nil
		}
		return response(http.StatusOK, frameBody("retry ok"), nil), nil
	}
	result, err := NewClient().Complete(context.Background(), Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"}, Request{Model: "gemini-pro", Prompt: "retry"}, do)
	if err != nil || result.Text != "retry ok" {
		t.Fatalf("retry result=%#v err=%v", result, err)
	}
	if gets != 2 || posts != 2 {
		t.Fatalf("gets=%d posts=%d, want 2 each", gets, posts)
	}
}

func frameBody(answer string) string {
	payload, _ := json.Marshal([]any{nil, []any{"cid", "rid"}, nil, nil, []any{[]any{nil, []any{answer}}}})
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", "req", string(payload)}})
	return ")]}'\n" + string(outer)
}

func response(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

package geminiweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
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

	snapshots := 0
	result, err := NewClient().Complete(context.Background(), Credential{Cookie: cookie, BaseURL: "https://gemini.test"}, Request{
		Model: "gemini-3.8-flash", Prompt: "hello",
		OnResult: func(snapshot Result) error {
			snapshots++
			if snapshot.Text != "hello from web" {
				t.Fatalf("stream snapshot text = %q", snapshot.Text)
			}
			return nil
		},
	}, do)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if result.Text != "hello from web" || result.CID != "cid" || result.RID != "rid" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if calls != 2 {
		t.Fatalf("request count = %d, want session + completion", calls)
	}
	if snapshots != 1 {
		t.Fatalf("stream snapshots = %d, want 1", snapshots)
	}
}

func TestClientCompleteEmitsSnapshotsBeforeUpstreamEOF(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	releaseFinal := make(chan struct{})
	go func() {
		first := frameBody("hello")
		// Deliberately split a frame across reads, as real HTTP streams do.
		_, _ = io.WriteString(writer, first[:len(first)/2])
		_, _ = io.WriteString(writer, first[len(first)/2:])
		select {
		case <-releaseFinal:
			_, _ = io.WriteString(writer, "\n"+frameBody("hello world"))
		case <-ctx.Done():
		}
		_ = writer.Close()
	}()
	do := func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader}, nil
	}
	snapshots := make(chan Result, 2)
	finished := make(chan error, 1)
	go func() {
		_, err := NewClient().Complete(ctx, Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"}, Request{
			Model: "gemini-pro", Prompt: "hello",
			OnResult: func(result Result) error {
				snapshots <- result
				return nil
			},
		}, do)
		finished <- err
	}()
	select {
	case snapshot := <-snapshots:
		if snapshot.Text != "hello" {
			t.Fatalf("first snapshot = %q, want hello", snapshot.Text)
		}
	case <-ctx.Done():
		t.Fatal("first snapshot was not delivered before upstream EOF")
	}
	close(releaseFinal)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("Complete() = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Complete() did not finish after upstream EOF")
	}
	select {
	case snapshot := <-snapshots:
		if snapshot.Text != "hello world" {
			t.Fatalf("last snapshot = %q, want hello world", snapshot.Text)
		}
	case <-ctx.Done():
		t.Fatal("missing final snapshot")
	}
}

type geminiWebEOFBody struct {
	data   string
	closed bool
}

func (b *geminiWebEOFBody) Read(p []byte) (int, error) {
	n := copy(p, b.data)
	b.data = b.data[n:]
	if b.data == "" {
		return n, io.EOF
	}
	return n, nil
}

func (b *geminiWebEOFBody) Close() error { b.closed = true; return nil }

func TestClientCompleteHandlesDataWithEOFAndCallbackFailure(t *testing.T) {
	callbackErr := errors.New("downstream write failed")
	for _, wantErr := range []error{nil, callbackErr} {
		t.Run(fmt.Sprint(wantErr), func(t *testing.T) {
			body := &geminiWebEOFBody{data: frameBody("final frame")}
			do := func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
			}
			calls := 0
			result, err := NewClient().Complete(context.Background(), Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"}, Request{
				Model: "gemini-pro", Prompt: "hello",
				OnResult: func(Result) error { calls++; return wantErr },
			}, do)
			if !errors.Is(err, wantErr) || result.Text != "final frame" || calls != 1 || !body.closed {
				t.Fatalf("result=%#v err=%v callbacks=%d closed=%v", result, err, calls, body.closed)
			}
		})
	}
}

func TestNormalizeCookieHeaderAcceptsBrowserJSONExport(t *testing.T) {
	input := `[{"name":"__Secure-1PSID","value":"psid","domain":".google.com","expirationDate":4102444800,"hostOnly":false},{"name":"__Secure-1PSIDTS","value":"ts","domain":".google.com","expirationDate":4102444800,"hostOnly":false},{"name":"COMPASS","value":"gemini-only","domain":".gemini.google.com","expirationDate":4102444800,"hostOnly":false},{"name":"expired","value":"old","domain":".google.com","expirationDate":1},{"name":"wrong_host","value":"secret","domain":"accounts.google.com","expirationDate":4102444800}]`
	got := NormalizeCookieHeader(input)
	if !strings.Contains(got, "__Secure-1PSID=psid") || !strings.Contains(got, "__Secure-1PSIDTS=ts") {
		t.Fatalf("normalized browser export = %q", got)
	}
	if strings.Contains(got, "COMPASS=") || strings.Contains(got, "expired=") || strings.Contains(got, "wrong_host=") {
		t.Fatalf("normalized browser export leaked expired or out-of-scope cookies: %q", got)
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

func TestClientExplicitAuthUserZeroUsesScopedGeminiPath(t *testing.T) {
	var bootstrapPath, bootstrapAuthUser string
	do := func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			bootstrapPath = req.URL.Path
			bootstrapAuthUser = req.Header.Get("X-Goog-AuthUser")
			return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
		}
		return response(http.StatusOK, frameBody("ok"), nil), nil
	}
	_, err := NewClient().Complete(context.Background(), Credential{
		Cookie: "__Secure-1PSID=psid", AuthUser: 0, AuthUserSet: true, BaseURL: "https://gemini.test",
	}, Request{Model: "gemini-pro", Prompt: "hello"}, do)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapPath != "/u/0/app" || bootstrapAuthUser != "0" {
		t.Fatalf("explicit auth_user=0 bootstrap = path %q, header %q", bootstrapPath, bootstrapAuthUser)
	}
}

func TestClientFetchQuotaParsesUsageWindows(t *testing.T) {
	calls := 0
	do := func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method == http.MethodGet {
			return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
		}
		if !strings.Contains(req.URL.Query().Get("rpcids"), "jSf9Qc") {
			t.Fatalf("quota RPC = %q", req.URL.Query().Get("rpcids"))
		}
		payload, _ := json.Marshal([]any{2, []any{
			[]any{"", "0.25", 1, []any{"1730000000", "0"}},
			[]any{"", "1", 2, []any{"1731000000", "0"}},
		}})
		outer, _ := json.Marshal([]any{[]any{"wrb.fr", "jSf9Qc", string(payload)}})
		return response(http.StatusOK, ")]}'\n"+string(outer), nil), nil
	}
	client := NewClient()
	cred := Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"}
	result, err := client.FetchQuota(context.Background(), cred, do)
	if err != nil {
		t.Fatalf("FetchQuota() error = %v", err)
	}
	if result.Quota.Plan != "PRO" || len(result.Quota.Windows) != 2 || !result.Quota.Windows[0].HasPercent {
		t.Fatalf("unexpected quota = %#v", result.Quota)
	}
	if result.Quota.Windows[0].RemainingPercent != 75 || !result.Quota.Windows[0].HasReset {
		t.Fatalf("unexpected five-hour window = %#v", result.Quota.Windows[0])
	}
	_, err = client.FetchQuota(context.Background(), cred, do)
	if err != nil || calls != 2 {
		t.Fatalf("quota result should be cached for one minute: calls=%d err=%v", calls, err)
	}
}

func TestClientRefreshCookiesUsesGoogleRotationEndpoint(t *testing.T) {
	client := NewClient()
	var got *http.Request
	do := func(req *http.Request) (*http.Response, error) {
		got = req
		if req.URL.Host != "accounts.google.com" || req.URL.Path != "/RotateCookies" {
			t.Fatalf("rotation target = %s, want accounts.google.com/RotateCookies", req.URL)
		}
		if req.Header.Get("Origin") != "https://accounts.google.com" || req.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("rotation headers missing: origin=%q content-type=%q", req.Header.Get("Origin"), req.Header.Get("Content-Type"))
		}
		return response(http.StatusOK, "[]", http.Header{"Set-Cookie": {
			"__Secure-1PSIDTS=renewed; Domain=.google.com; Path=/; Secure",
			"accounts_only=1; Domain=accounts.google.com; Path=/; Secure",
		}}), nil
	}
	updated, err := client.RefreshCookies(context.Background(), Credential{Cookie: "__Secure-1PSID=psid; __Secure-1PSIDTS=old"}, do)
	if err != nil {
		t.Fatalf("RefreshCookies() error = %v", err)
	}
	if got == nil || !strings.Contains(got.Header.Get("Cookie"), "__Secure-1PSIDTS=old") {
		t.Fatal("rotation did not send the existing timestamp cookie")
	}
	if !strings.Contains(updated, "__Secure-1PSIDTS=renewed") {
		t.Fatalf("rotated cookie = %q", updated)
	}
	if strings.Contains(updated, "accounts_only=") {
		t.Fatalf("host-only account cookie leaked into Gemini cookie header: %q", updated)
	}
}

func TestMergeSetCookieHonorsDeletionAttributes(t *testing.T) {
	current := "__Secure-1PSID=psid; __Secure-1PSIDTS=old; stale=value"
	updated := mergeSetCookie(current, []string{
		"__Secure-1PSIDTS=renewed; Domain=.google.com; Path=/; Secure",
		"stale=; Domain=.google.com; Path=/; Max-Age=0",
	})
	if strings.Contains(updated, "stale=") || !strings.Contains(updated, "__Secure-1PSIDTS=renewed") {
		t.Fatalf("merged cookie = %q", updated)
	}
}

func TestGoogleSharedCookieValuesRejectsSubdomainCookies(t *testing.T) {
	values := googleSharedCookieValues([]string{
		"shared=1; Domain=.google.com; Path=/",
		"accounts_only=1; Domain=accounts.google.com; Path=/",
		"gemini_only=1; Domain=gemini.google.com; Path=/",
		"host_only=1; Path=/",
	})
	joined := strings.Join(values, "\n")
	if !strings.Contains(joined, "shared=1") || strings.Contains(joined, "accounts_only=") || strings.Contains(joined, "gemini_only=") || strings.Contains(joined, "host_only=") {
		t.Fatalf("filtered cookies = %q", joined)
	}
}

func TestClientLazyCookieKeepaliveRotatesCachedSession(t *testing.T) {
	client := NewClient()
	cred := Credential{Cookie: "__Secure-1PSID=psid; __Secure-1PSIDTS=old", BaseURL: "https://gemini.test"}
	var appCalls, rotateCalls, generateCalls int
	do := func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Path == "/app":
			appCalls++
			return response(http.StatusOK, `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil), nil
		case req.URL.Host == "accounts.google.com" && req.URL.Path == "/RotateCookies":
			rotateCalls++
			return response(http.StatusOK, "[]", http.Header{"Set-Cookie": {"__Secure-1PSIDTS=renewed; Domain=.google.com; Path=/; Secure"}}), nil
		default:
			generateCalls++
			if generateCalls == 2 && !strings.Contains(req.Header.Get("Cookie"), "__Secure-1PSIDTS=renewed") {
				t.Fatalf("generation did not use rotated Cookie: %q", req.Header.Get("Cookie"))
			}
			return response(http.StatusOK, frameBody("hello"), nil), nil
		}
	}
	if _, err := client.Complete(context.Background(), cred, Request{Model: "gemini-pro", Prompt: "first"}, do); err != nil {
		t.Fatal(err)
	}
	key := credentialKey(cred.Cookie, cred.AuthUser, cred.AuthUserSet)
	client.mu.Lock()
	s := client.sessions[key]
	s.CookieRefreshed = time.Now().Add(-geminiWebCookieRefreshInterval - time.Second)
	client.sessions[key] = s
	client.mu.Unlock()
	result, err := client.Complete(context.Background(), cred, Request{Model: "gemini-pro", Prompt: "second"}, do)
	if err != nil || result.Text != "hello" {
		t.Fatalf("second Complete() result=%#v err=%v", result, err)
	}
	if appCalls != 1 || rotateCalls != 1 || generateCalls != 2 {
		t.Fatalf("app=%d rotate=%d generate=%d, want 1/1/2", appCalls, rotateCalls, generateCalls)
	}
}

func TestClientStaleCookieResponseCannotRollBackLiveSession(t *testing.T) {
	client := NewClient()
	key := credentialKey("__Secure-1PSID=psid; __Secure-1PSIDTS=old", 0, false)
	client.sessions[key] = session{Cookie: "__Secure-1PSID=psid; __Secure-1PSIDTS=old"}
	client.updateSessionCookie(Credential{Cookie: "__Secure-1PSID=psid; __Secure-1PSIDTS=old"}, "__Secure-1PSID=psid; __Secure-1PSIDTS=new")
	client.updateSessionCookie(Credential{Cookie: "__Secure-1PSID=psid; __Secure-1PSIDTS=old"}, "__Secure-1PSID=psid; __Secure-1PSIDTS=old")
	if got := client.sessions[key].Cookie; !strings.Contains(got, "__Secure-1PSIDTS=new") {
		t.Fatalf("stale response rolled back live Cookie: %q", got)
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

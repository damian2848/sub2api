//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cannedFirstGeminiWebUpstream answers the first Gemini Web account it sees
// with Gemini's canned failure message and every other account normally.
type cannedFirstGeminiWebUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	first    int64
	accounts []int64
	// allCanned makes every account return the canned reply.
	allCanned bool
}

func geminiWebHandlerFrames(text string) string {
	payload := make([]any, 43)
	payload[4] = []any{[]any{"candidate", []any{text}}}
	b, _ := json.Marshal(payload)
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(b)}})
	return string(outer) + "\n"
}

func (u *cannedFirstGeminiWebUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
	if req.Method == http.MethodPost {
		u.mu.Lock()
		if u.first == 0 {
			u.first = accountID
		}
		u.accounts = append(u.accounts, accountID)
		canned := u.allCanned || accountID == u.first
		u.mu.Unlock()
		text := "Writing.\n<tool_call>\n{\"name\": \"write_file\", \"arguments\": {\"path\": \"pelican.html\"}}\n</tool_call>"
		if canned {
			text = "I seem to be encountering an error. Can I try something else for you?"
		}
		body = geminiWebHandlerFrames(text)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func newGeminiWebCannedHandler(t *testing.T, upstream *cannedFirstGeminiWebUpstream) *geminiClientCancelFixture {
	t.Helper()
	// Canned replies save raw captures under DATA_DIR in the background; keep
	// them out of the repo. Removal is best effort because a capture may land
	// after the test ends.
	dataDir, err := os.MkdirTemp("", "sub2api-handler-gemini-web-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	t.Setenv("DATA_DIR", dataDir)
	f := newGeminiClientCancelFixture(t)
	f.ctx = context.Background()
	groupID := f.group.ID
	var accounts []*service.Account
	for _, id := range []int64{9301, 9302} {
		accounts = append(accounts, &service.Account{
			ID: id, Platform: service.PlatformGemini, Type: service.AccountTypeGeminiWeb,
			Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid-" + string(rune('a'+id-9301))},
			Status:      service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1,
			AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}},
		})
	}
	h, cleanup := newTestGatewayHandler(t, f.group, accounts)
	t.Cleanup(cleanup)
	h.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, upstream, nil, &config.Config{})
	f.handler = h
	return f
}

// When every account returns the canned reply, the client gets an explicit
// error saying the request was not billed, never the canned text.
func TestGatewayGeminiWebCannedReplyOnAllAccountsReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, route, path, body string
		call                    func(*GatewayHandler) gin.HandlerFunc
		wantStatus              int
	}{
		{"messages-stream", "/v1/messages", "/v1/messages",
			`{"model":"gemini-4-argon","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			func(h *GatewayHandler) gin.HandlerFunc { return h.Messages }, http.StatusBadGateway},
		{"chat-stream", "/v1/chat/completions", "/v1/chat/completions",
			`{"model":"gemini-4-argon","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			func(h *GatewayHandler) gin.HandlerFunc { return h.ChatCompletions }, http.StatusBadGateway},
		{"chat", "/v1/chat/completions", "/v1/chat/completions",
			`{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`,
			func(h *GatewayHandler) gin.HandlerFunc { return h.ChatCompletions }, http.StatusBadGateway},
		{"native", "/v1beta/models/*modelAction", "/v1beta/models/gemini-4-argon:generateContent",
			`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			func(h *GatewayHandler) gin.HandlerFunc { return h.GeminiV1BetaModels }, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &cannedFirstGeminiWebUpstream{allCanned: true}
			f := newGeminiWebCannedHandler(t, upstream)
			rec, _ := f.serve(t, tc.route, tc.path, tc.body, tc.call(f.handler))
			out := rec.Body.String()
			require.Len(t, upstream.accounts, 2, "both accounts were tried")
			require.Equal(t, tc.wantStatus, rec.Code, out)
			require.Contains(t, out, "this request was not billed", out)
			require.NotContains(t, out, "encountering an error")
		})
	}
}

func TestGatewayGeminiWebCannedReplyFailsOverToAnotherAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, route, body string
		call              func(*GatewayHandler) gin.HandlerFunc
	}{
		{"messages", "/v1/messages",
			`{"model":"gemini-4-argon","stream":true,"max_tokens":64,"tools":[{"name":"write_file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],"messages":[{"role":"user","content":"draw a pelican"}]}`,
			func(h *GatewayHandler) gin.HandlerFunc { return h.Messages }},
		{"chat", "/v1/chat/completions",
			`{"model":"gemini-4-argon","stream":true,"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],"messages":[{"role":"user","content":"draw a pelican"}]}`,
			func(h *GatewayHandler) gin.HandlerFunc { return h.ChatCompletions }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &cannedFirstGeminiWebUpstream{}
			f := newGeminiWebCannedHandler(t, upstream)
			h := f.handler

			rec, _ := f.serve(t, tc.route, tc.route, tc.body, tc.call(h))

			require.Len(t, upstream.accounts, 2, "the canned reply switched to the other account")
			require.NotEqual(t, upstream.accounts[0], upstream.accounts[1])
			out := rec.Body.String()
			require.Equal(t, http.StatusOK, rec.Code, out)
			require.NotContains(t, out, "encountering an error", "the canned reply never reaches the client")
			require.Contains(t, out, "pelican.html")
			require.Equal(t, 1, strings.Count(out, "Writing."), out)
		})
	}
}

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

type keepaliveAccountRepoStub struct {
	AccountRepository
	account     Account
	updated     map[string]any
	updateCalls int
}

func (r *keepaliveAccountRepoStub) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	if platform != PlatformGemini {
		return nil, nil
	}
	return []Account{r.account}, nil
}

func (r *keepaliveAccountRepoStub) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	if id != r.account.ID {
		return nil
	}
	r.updated = credentials
	r.account.Credentials = credentials
	r.updateCalls++
	return nil
}

type keepaliveHTTPStub struct {
	HTTPUpstream
	requests int
}

func (h *keepaliveHTTPStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	h.requests++
	if req.URL.Host != "accounts.google.com" || req.URL.Path != "/RotateCookies" {
		return nil, &keepaliveUnexpectedRequestError{url: req.URL.String()}
	}
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Header: http.Header{"Set-Cookie": {"__Secure-1PSIDTS=renewed; Domain=.google.com; Path=/; Secure"}},
		Body:   io.NopCloser(strings.NewReader("[]")),
	}, nil
}

func (h *keepaliveHTTPStub) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	return nil, nil
}

type keepaliveUnexpectedRequestError struct{ url string }

func (e *keepaliveUnexpectedRequestError) Error() string { return "unexpected request: " + e.url }

func TestGeminiWebCookieKeepalivePersistsRotatedCookie(t *testing.T) {
	repo := &keepaliveAccountRepoStub{account: Account{
		ID: 42, Platform: PlatformGemini, Type: AccountTypeGeminiWeb,
		Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid; __Secure-1PSIDTS=old"},
	}}
	upstream := &keepaliveHTTPStub{}
	service := NewGeminiWebCookieKeepaliveService(repo, upstream)
	service.runOnce(context.Background())
	if upstream.requests != 1 {
		t.Fatalf("rotation requests = %d, want 1", upstream.requests)
	}
	if repo.updateCalls != 1 || !strings.Contains(repo.updated["gemini_web_cookie"].(string), "__Secure-1PSIDTS=renewed") || repo.updated[geminiWebCookieLastRefresh] == nil {
		t.Fatalf("rotated credentials were not persisted: calls=%d credentials=%#v", repo.updateCalls, repo.updated)
	}
	service.runOnce(context.Background())
	if upstream.requests != 1 {
		t.Fatalf("recently refreshed account was rotated again: requests=%d", upstream.requests)
	}
}

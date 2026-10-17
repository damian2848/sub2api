package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

// fetchGeminiWebUpstreamModels reads the models the account's Gemini Web
// picker offers right now. The built-in table also lists models an account may
// not have (and Google silently downgrades those), so sync keeps only the
// models the startup page lists.
func (s *AccountTestService) fetchGeminiWebUpstreamModels(ctx context.Context, account *Account) ([]string, []byte, error) {
	if s.httpUpstream == nil {
		return nil, nil, newUpstreamModelSyncConfigError("Upstream HTTP client is not configured", nil)
	}
	cookie := account.GetCredential("gemini_web_cookie")
	if strings.TrimSpace(cookie) == "" {
		return nil, nil, newUpstreamModelSyncConfigError("Gemini Web cookie is not configured", nil)
	}
	authUser, authUserSet := geminiWebAuthUser(account)
	cred := geminiweb.Credential{
		Cookie: cookie, AuthUser: authUser, AuthUserSet: authUserSet,
		BaseURL: "https://gemini.google.com", UserAgent: account.GetCredential("user_agent"),
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	do := func(req *http.Request) (*http.Response, error) {
		return s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	}
	// A fresh client so sync always reflects the current startup page.
	catalog, err := geminiweb.NewClient().AvailableModels(ctx, cred, do)
	if catalog.UpdatedCookie != "" && !errors.Is(err, geminiweb.ErrInvalidCredential) {
		updated := shallowCopyMap(account.Credentials)
		updated["gemini_web_cookie"] = catalog.UpdatedCookie
		if persistErr := persistAccountCredentials(ctx, s.accountRepo, account, updated); persistErr != nil {
			account.Credentials = updated
		}
	}
	switch {
	case errors.Is(err, geminiweb.ErrInvalidCredential):
		return nil, nil, newUpstreamModelSyncUpstreamError("Gemini Web cookie expired or was rejected", err)
	case errors.Is(err, geminiweb.ErrRateLimited):
		return nil, nil, newUpstreamModelSyncUpstreamError("Gemini Web is rate limited", err)
	case errors.Is(err, geminiweb.ErrCatalogUnavailable):
		return nil, nil, newUpstreamModelSyncUpstreamError("Gemini Web startup page did not list any models", err)
	case err != nil:
		return nil, nil, newUpstreamModelSyncUpstreamError("Gemini Web model discovery failed", err)
	}
	if len(catalog.UnknownIDs) > 0 {
		slog.Info("gemini web picker lists models missing from the built-in table",
			"account_id", account.ID, "unknown_ids", catalog.UnknownIDs)
	}
	ids := make([]string, 0, len(catalog.Models))
	for _, model := range catalog.Models {
		ids = append(ids, model.ID)
	}
	return ids, nil, nil
}

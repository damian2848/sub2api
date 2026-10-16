package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

const geminiWebQuotaPrecheckTimeout = 15 * time.Second

// passesGeminiWebQuotaPreCheck reads Gemini Web's account-wide usage windows
// before candidate selection. Transport/decoding failures fail open, while an
// invalid cookie or an explicit upstream rate limit removes the account.
func (s *GeminiMessagesCompatService) passesGeminiWebQuotaPreCheck(ctx context.Context, account *Account) bool {
	if s == nil || account == nil || !account.IsGeminiWeb() || s.httpUpstream == nil {
		return true
	}
	cookie := account.GetCredential("gemini_web_cookie")
	if strings.TrimSpace(cookie) == "" {
		return false
	}
	authUser, authUserSet := geminiWebAuthUser(account)
	cred := geminiweb.Credential{Cookie: cookie, AuthUser: authUser, AuthUserSet: authUserSet, BaseURL: "https://gemini.google.com", UserAgent: account.GetCredential("user_agent")}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	do := func(req *http.Request) (*http.Response, error) {
		return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
	quotaCtx, cancel := context.WithTimeout(ctx, geminiWebQuotaPrecheckTimeout)
	defer cancel()
	result, err := s.geminiWebClient().FetchQuota(quotaCtx, cred, do)
	if live := s.geminiWebClient().CurrentCookie(cred); live != "" {
		result.UpdatedCookie = live
	}
	if !errors.Is(err, geminiweb.ErrInvalidCredential) && !errors.Is(err, geminiweb.ErrRateLimited) {
		s.persistGeminiWebQuotaCookie(ctx, account, cookie, result.UpdatedCookie)
	}
	if err != nil {
		s.handleGeminiWebQuotaError(ctx, account, cookie, err)
		return !errors.Is(err, geminiweb.ErrInvalidCredential) && !errors.Is(err, geminiweb.ErrRateLimited)
	}
	return geminiWebQuotaAvailable(result.Quota, time.Now())
}
func geminiWebQuotaAvailable(quota geminiweb.Quota, now time.Time) bool {
	for _, w := range quota.Windows {
		if !w.HasPercent || w.RemainingPercent > 0 {
			continue
		}
		if w.HasReset && !w.ResetAt.After(now) {
			continue
		}
		return false
	}
	return true
}
func (s *GeminiMessagesCompatService) persistGeminiWebQuotaCookie(ctx context.Context, account *Account, oldCookie, updatedCookie string) {
	if account == nil || strings.TrimSpace(updatedCookie) == "" || updatedCookie == oldCookie {
		return
	}
	updated := shallowCopyMap(account.Credentials)
	updated["gemini_web_cookie"] = updatedCookie
	updated[geminiWebCookieLastRefresh] = time.Now().UTC().Format(time.RFC3339Nano)
	persistLock := geminiWebCredentialPersistLock(account.ID)
	persistLock.Lock()
	defer persistLock.Unlock()
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := persistAccountCredentials(persistCtx, s.accountRepo, account, updated); err != nil {
		account.Credentials = updated
	}
}
func (s *GeminiMessagesCompatService) handleGeminiWebQuotaError(ctx context.Context, account *Account, cookie string, err error) {
	if errors.Is(err, geminiweb.ErrInvalidCredential) {
		s.geminiWebClient().Invalidate(cookie)
		if s.accountRepo != nil && account.ID > 0 {
			until := time.Now().Add(10 * time.Minute)
			_ = s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, "Gemini Web cookie expired or rejected")
			account.TempUnschedulableUntil = &until
		}
		return
	}
	if errors.Is(err, geminiweb.ErrRateLimited) && s.accountRepo != nil && account.ID > 0 {
		now := time.Now()
		resetAt := now.Add(time.Minute)
		_ = s.accountRepo.SetRateLimited(ctx, account.ID, resetAt)
		account.RateLimitedAt = &now
		account.RateLimitResetAt = &resetAt
	}
}

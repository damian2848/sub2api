package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

// Quota is a scheduling hint and must not consume the user's first-token
// budget. A slow usage RPC fails open; the generation request still validates
// the cookie and records the final usage normally.
const geminiWebQuotaPrecheckTimeout = 3 * time.Second

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
	now := time.Now()
	applyGeminiWebQuotaRateLimit(ctx, s.accountRepo, account, result.Quota, now)
	return geminiWebQuotaAvailable(result.Quota, now)
}
func geminiWebQuotaAvailable(quota geminiweb.Quota, now time.Time) bool {
	for _, w := range quota.Windows {
		if !geminiWebWindowExhausted(w) {
			continue
		}
		if w.HasReset && !w.ResetAt.After(now) {
			continue
		}
		return false
	}
	return true
}

func geminiWebWindowExhausted(w geminiweb.QuotaWindow) bool {
	return w.HasPercent && w.RemainingPercent <= 0
}

// geminiWebQuotaRateLimitUntil returns when the account may be scheduled again
// after a usage window reached 100%: the latest future refresh time among the
// exhausted windows. A window without a usable refresh time sets no deadline;
// the scheduler precheck still skips it while it reads as exhausted.
func geminiWebQuotaRateLimitUntil(quota geminiweb.Quota, now time.Time) (time.Time, bool) {
	var until time.Time
	for _, w := range quota.Windows {
		if !geminiWebWindowExhausted(w) || !w.HasReset || !w.ResetAt.After(now) {
			continue
		}
		if w.ResetAt.After(until) {
			until = w.ResetAt
		}
	}
	return until, !until.IsZero()
}

// geminiWebQuotaRecoveryMinLimit separates quota-derived rate limits from the
// one-minute 429 cooldowns: only limits at least this long are lifted early
// when the usage page shows every window below 100% again.
const geminiWebQuotaRecoveryMinLimit = 5 * time.Minute

// applyGeminiWebQuotaRateLimit turns an exhausted Gemini Web usage window into
// the account's regular rate limit, ending at that window's refresh time, so
// the scheduler stops offering the account until then. When the windows have
// room again before that time, a quota-derived limit is cleared.
func applyGeminiWebQuotaRateLimit(ctx context.Context, repo AccountRepository, account *Account, quota geminiweb.Quota, now time.Time) {
	if repo == nil || account == nil || account.ID <= 0 {
		return
	}
	// The usage read already succeeded; a caller that hangs up now must not
	// leave the account schedulable on an exhausted window.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	until, exhausted := geminiWebQuotaRateLimitUntil(quota, now)
	if !exhausted {
		if geminiWebQuotaAvailable(quota, now) && geminiWebQuotaRateLimitActive(account, now) {
			if err := repo.ClearRateLimit(ctx, account.ID); err != nil {
				slog.Warn("gemini_web_quota_rate_limit_clear_failed", "account_id", account.ID, "error", err)
				return
			}
			account.RateLimitedAt = nil
			account.RateLimitResetAt = nil
			slog.Info("gemini_web_quota_rate_limit_cleared", "account_id", account.ID)
		}
		return
	}
	if account.RateLimitResetAt != nil && !account.RateLimitResetAt.Before(until) {
		return
	}
	var err error
	if extendingRepo, ok := repo.(grokRateLimitExtendingRepository); ok {
		err = extendingRepo.SetRateLimitedIfLater(ctx, account.ID, until)
	} else {
		err = repo.SetRateLimited(ctx, account.ID, until)
	}
	if err != nil {
		slog.Warn("gemini_web_quota_rate_limit_set_failed", "account_id", account.ID, "reset_at", until, "error", err)
		return
	}
	account.RateLimitedAt = &now
	account.RateLimitResetAt = &until
	slog.Info("gemini_web_quota_rate_limited", "account_id", account.ID, "reset_at", until, "reset_in", until.Sub(now).Truncate(time.Second))
}

func geminiWebQuotaRateLimitActive(account *Account, now time.Time) bool {
	if account.RateLimitedAt == nil || account.RateLimitResetAt == nil || !account.RateLimitResetAt.After(now) {
		return false
	}
	return account.RateLimitResetAt.Sub(*account.RateLimitedAt) >= geminiWebQuotaRecoveryMinLimit
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

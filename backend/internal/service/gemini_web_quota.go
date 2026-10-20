package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

// Quota is a scheduling hint and must not consume the user's first-token
// budget. A slow usage RPC fails open; the generation request still validates
// the cookie and records the final usage normally.
const geminiWebQuotaPrecheckTimeout = 3 * time.Second

// precheckGeminiWebGenerationQuota validates only the selected account, using
// the client's quota cache and a bounded refresh on a cache miss. An unavailable
// usage endpoint fails open, but a known exhausted window is persisted before
// the caller selects another account.
func (s *GeminiMessagesCompatService) precheckGeminiWebGenerationQuota(ctx context.Context, account *Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || account == nil || !account.IsGeminiWeb() {
		return nil
	}
	if strings.TrimSpace(account.GetCredential("gemini_web_cookie")) == "" {
		return geminiweb.ErrInvalidCredential
	}
	if s.httpUpstream == nil {
		return nil
	}
	quota, err := s.readGeminiWebQuota(ctx, account, geminiWebQuotaPrecheckTimeout, false)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, geminiweb.ErrInvalidCredential) || errors.Is(err, geminiweb.ErrRateLimited) {
			return err
		}
		return nil
	}
	now := time.Now()
	applyGeminiWebQuotaRateLimit(ctx, s.accountRepo, account, quota, now)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !geminiWebQuotaAvailable(quota, now) {
		return geminiweb.ErrRateLimited
	}
	return nil
}

// passesGeminiWebQuotaPreCheck reads Gemini Web's account-wide usage windows
// before candidate selection. Transport/decoding failures fail open, while an
// invalid cookie or an explicit upstream rate limit removes the account.
func (s *GeminiMessagesCompatService) passesGeminiWebQuotaPreCheck(ctx context.Context, account *Account) bool {
	if s == nil || account == nil || !account.IsGeminiWeb() || s.httpUpstream == nil {
		return true
	}
	if strings.TrimSpace(account.GetCredential("gemini_web_cookie")) == "" {
		return false
	}
	quota, err := s.readGeminiWebQuota(ctx, account, geminiWebQuotaPrecheckTimeout, false)
	if err != nil {
		return !errors.Is(err, geminiweb.ErrInvalidCredential) && !errors.Is(err, geminiweb.ErrRateLimited)
	}
	now := time.Now()
	applyGeminiWebQuotaRateLimit(ctx, s.accountRepo, account, quota, now)
	return geminiWebQuotaAvailable(quota, now)
}

// readGeminiWebQuota fetches the usage windows, keeps the rotated cookie and
// applies the cookie / 429 side effects of a failed read.
func (s *GeminiMessagesCompatService) readGeminiWebQuota(ctx context.Context, account *Account, timeout time.Duration, force bool) (geminiweb.Quota, error) {
	cookie := account.GetCredential("gemini_web_cookie")
	if strings.TrimSpace(cookie) == "" {
		return geminiweb.Quota{}, geminiweb.ErrInvalidCredential
	}
	authUser, authUserSet := geminiWebAuthUser(account)
	cred := geminiweb.Credential{CacheGeneration: account.GetCredential(geminiWebCookieGeneration), Cookie: cookie, AuthUser: authUser, AuthUserSet: authUserSet, BaseURL: "https://gemini.google.com", UserAgent: account.GetCredential("user_agent")}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	do := func(req *http.Request) (*http.Response, error) {
		return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
	quotaCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var result geminiweb.QuotaResult
	var err error
	if force {
		result, err = s.geminiWebClient().RefreshQuota(quotaCtx, cred, do)
	} else {
		result, err = s.geminiWebClient().FetchQuota(quotaCtx, cred, do)
	}
	if live := s.geminiWebClient().CurrentCookie(cred); live != "" {
		result.UpdatedCookie = live
	}
	if !errors.Is(err, geminiweb.ErrInvalidCredential) && !errors.Is(err, geminiweb.ErrRateLimited) {
		s.persistGeminiWebQuotaCookie(ctx, account, cookie, result.UpdatedCookie)
	}
	if err != nil {
		s.handleGeminiWebQuotaError(ctx, account, cookie, err)
		return geminiweb.Quota{}, err
	}
	return result.Quota, nil
}

const (
	// geminiWebQuotaRefreshInterval caps how often one account's usage page is
	// re-read after its requests; bursts share a single read.
	geminiWebQuotaRefreshInterval = 15 * time.Second
	geminiWebQuotaRefreshTimeout  = 20 * time.Second
)

// geminiWebQuotaRefreshGate admits at most one post-request usage read per
// account per geminiWebQuotaRefreshInterval.
type geminiWebQuotaRefreshGate struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

func (g *geminiWebQuotaRefreshGate) allow(accountID int64, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = make(map[int64]time.Time)
	}
	if last, ok := g.last[accountID]; ok && now.Sub(last) < geminiWebQuotaRefreshInterval {
		return false
	}
	g.last[accountID] = now
	return true
}

// refreshGeminiWebQuotaAfterRequest re-reads the usage windows in the
// background after a Gemini Web call, so an account whose call used up a
// window is rate limited for every scheduler, not only the ones that run the
// quota precheck.
func (s *GeminiMessagesCompatService) refreshGeminiWebQuotaAfterRequest(account *Account) {
	if s == nil || account == nil || account.ID <= 0 || s.accountRepo == nil || s.httpUpstream == nil {
		return
	}
	if !s.geminiWebQuotaRefresh.allow(account.ID, time.Now()) {
		return
	}
	// The request goroutine keeps using its account; the read works on a copy.
	snapshot := *account
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), geminiWebQuotaRefreshTimeout)
		defer cancel()
		quota, err := s.readGeminiWebQuota(ctx, &snapshot, geminiWebQuotaRefreshTimeout, true)
		if err != nil {
			return
		}
		applyGeminiWebQuotaRateLimit(ctx, s.accountRepo, &snapshot, quota, time.Now())
	}()
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
// exhausted windows. Without a refresh time, a short deadline prevents every
// request from rediscovering the same exhausted account; expiry allows a recheck.
func geminiWebQuotaRateLimitUntil(quota geminiweb.Quota, now time.Time) (time.Time, bool) {
	var until time.Time
	for _, w := range quota.Windows {
		if !geminiWebWindowExhausted(w) {
			continue
		}
		resetAt := w.ResetAt
		if !w.HasReset {
			resetAt = now.Add(time.Minute)
		}
		if resetAt.After(now) && resetAt.After(until) {
			until = resetAt
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
			if !quota.ObservedAt.IsZero() && quota.ObservedAt.Before(*account.RateLimitedAt) {
				// A positive cache entry from before the current ban is not
				// evidence that the account has recovered.
				return
			}
			recoveryRepo, ok := repo.(grokRateLimitRecoveryRepository)
			if !ok {
				// A delayed usage read must not clear a newer limit without
				// checking the generation it observed before the read.
				return
			}
			cleared, err := recoveryRepo.ClearRateLimitIfObserved(ctx, account.ID, *account.RateLimitedAt, *account.RateLimitResetAt)
			if err != nil {
				slog.Warn("gemini_web_quota_rate_limit_clear_failed", "account_id", account.ID, "error", err)
				return
			}
			if !cleared {
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

// applyGeminiWebShortRateLimit records a transient 429 without replacing a
// longer quota window. The repository's monotonic write also protects against
// another request setting a longer limit after this account snapshot was read.
func applyGeminiWebShortRateLimit(ctx context.Context, repo AccountRepository, account *Account) {
	if repo == nil || account == nil || account.ID <= 0 {
		return
	}
	now := time.Now()
	until := now.Add(time.Minute)
	if account.RateLimitResetAt != nil && !account.RateLimitResetAt.Before(until) {
		return
	}
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var err error
	if extendingRepo, ok := repo.(grokRateLimitExtendingRepository); ok {
		err = extendingRepo.SetRateLimitedIfLater(stateCtx, account.ID, until)
	} else {
		err = repo.SetRateLimited(stateCtx, account.ID, until)
	}
	if err != nil {
		slog.Warn("gemini_web_short_rate_limit_set_failed", "account_id", account.ID, "error", err)
		return
	}
	account.RateLimitedAt = &now
	account.RateLimitResetAt = &until
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
		s.geminiWebClient().InvalidateCredential(geminiWebCacheCredential(account, cookie))
		if s.accountRepo != nil && account.ID > 0 {
			markGeminiWebCookieRejected(ctx, s.accountRepo, account, "Gemini Web cookie expired or rejected")
		}
		return
	}
	if errors.Is(err, geminiweb.ErrRateLimited) {
		applyGeminiWebShortRateLimit(ctx, s.accountRepo, account)
	}
}

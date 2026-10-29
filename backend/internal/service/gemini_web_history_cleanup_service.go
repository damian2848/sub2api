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

const (
	geminiWebHistoryScanInterval      = 5 * time.Minute
	geminiWebHistoryPollInterval      = 15 * time.Second
	geminiWebHistoryLeaseDuration     = 90 * time.Second
	geminiWebHistoryHeartbeatInterval = 30 * time.Second
	geminiWebHistoryHTTPTimeout       = 20 * time.Second
	geminiWebHistoryDBTimeout         = 5 * time.Second
)

type geminiWebHistoryClient interface {
	ListConversations(context.Context, geminiweb.Credential, geminiweb.Doer) (geminiweb.HistoryResult, error)
	DeleteConversation(context.Context, geminiweb.Credential, string, geminiweb.Doer) (string, error)
	CurrentCookie(geminiweb.Credential) string
}

// GeminiWebHistoryCleanupService runs one serial worker. Durable account gates
// prevent new generations while a claimed job waits for existing requests and
// removes only the conversation snapshot recorded for that job.
type GeminiWebHistoryCleanupService struct {
	accountRepo                                                  AccountRepository
	jobs                                                         GeminiWebHistoryCleanupRepository
	compat                                                       *GeminiMessagesCompatService
	client                                                       geminiWebHistoryClient
	quotaRead                                                    func(context.Context, *Account) (geminiweb.Quota, error)
	quotaApply                                                   func(context.Context, *Account, geminiweb.Quota, time.Time)
	cookiePersist                                                func(context.Context, *Account, string, string)
	now                                                          func() time.Time
	scanInterval, pollInterval, heartbeatInterval, drainInterval time.Duration
	ctx                                                          context.Context
	cancel                                                       context.CancelFunc
	wg                                                           sync.WaitGroup
	start, stop                                                  sync.Once
}

func NewGeminiWebHistoryCleanupService(accountRepo AccountRepository, geminiCompat *GeminiMessagesCompatService) *GeminiWebHistoryCleanupService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &GeminiWebHistoryCleanupService{
		accountRepo: accountRepo, compat: geminiCompat, ctx: ctx, cancel: cancel,
		now: time.Now, scanInterval: geminiWebHistoryScanInterval, pollInterval: geminiWebHistoryPollInterval,
		heartbeatInterval: geminiWebHistoryHeartbeatInterval, drainInterval: 2 * time.Second,
	}
	s.jobs, _ = accountRepo.(GeminiWebHistoryCleanupRepository)
	if geminiCompat != nil {
		s.client = geminiCompat.geminiWebClient()
		s.quotaRead = func(ctx context.Context, account *Account) (geminiweb.Quota, error) {
			return geminiCompat.readGeminiWebQuota(ctx, account, geminiWebHistoryHTTPTimeout, true)
		}
		s.cookiePersist = geminiCompat.persistGeminiWebQuotaCookie
	}
	s.quotaApply = func(ctx context.Context, account *Account, quota geminiweb.Quota, now time.Time) {
		applyGeminiWebQuotaRateLimit(ctx, accountRepo, account, quota, now)
	}
	return s
}

func (s *GeminiWebHistoryCleanupService) Start() {
	if s == nil || s.jobs == nil || s.client == nil || s.compat == nil || s.compat.httpUpstream == nil {
		return
	}
	s.start.Do(func() {
		s.wg.Add(2)
		go func() {
			defer s.wg.Done()
			s.scanAccounts(s.ctx)
			scan := time.NewTicker(s.scanInterval)
			defer scan.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-scan.C:
					s.scanAccounts(s.ctx)
				}
			}
		}()
		go func() {
			defer s.wg.Done()
			s.pollJob(s.ctx)
			poll := time.NewTicker(s.pollInterval)
			defer poll.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-poll.C:
					s.pollJob(s.ctx)
				}
			}
		}()
	})
}

func (s *GeminiWebHistoryCleanupService) Stop() {
	if s == nil {
		return
	}
	s.stop.Do(s.cancel)
	s.wg.Wait()
}

func geminiWebHistoryAccountActive(account *Account, now time.Time) bool {
	return account != nil && account.IsGeminiWeb() && account.IsActive() && account.Schedulable &&
		(account.ExpiresAt == nil || account.ExpiresAt.After(now))
}

func (s *GeminiWebHistoryCleanupService) scanAccounts(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	listCtx, cancel := context.WithTimeout(ctx, geminiWebHistoryDBTimeout)
	accounts, err := s.accountRepo.ListByPlatform(listCtx, PlatformGemini)
	cancel()
	if err != nil {
		return
	}
	// Bounded parallel reads prevent a slow account delaying every other
	// account's detection. The independent job loop can consume queued work.
	queue := make(chan *Account)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for account := range queue {
				if ctx.Err() != nil {
					continue
				}
				quota, err := s.quotaRead(ctx, account)
				if err == nil && ctx.Err() == nil {
					s.quotaApply(ctx, account, quota, s.now())
				}
			}
		}()
	}
enqueue:
	for i := range accounts {
		account := &accounts[i]
		if !geminiWebHistoryAccountActive(account, s.now()) || geminiWebHistoryIdentity(account) == "" {
			continue
		}
		select {
		case queue <- account:
		case <-ctx.Done():
			break enqueue
		}
	}
	close(queue)
	readers.Wait()
}

func (s *GeminiWebHistoryCleanupService) pollJob(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	claimCtx, cancel := context.WithTimeout(ctx, geminiWebHistoryDBTimeout)
	job, err := s.jobs.ClaimGeminiWebHistoryCleanup(claimCtx, geminiWebHistoryLeaseDuration)
	cancel()
	if err != nil || job == nil {
		return
	}
	s.runJob(ctx, job)
}

type geminiWebHistoryOutcome struct {
	code   string
	cancel bool
	count  int
}

func (s *GeminiWebHistoryCleanupService) runJob(parent context.Context, job *GeminiWebHistoryCleanupJob) {
	ctx, cancel := context.WithCancelCause(parent)
	leaseJob := *job
	// Repository renewal may update LeaseUntil; use independent copies to
	// avoid a heartbeat racing with the foreground job's immutable snapshot.
	renew := func() bool {
		if ctx.Err() != nil {
			return false
		}
		copy := leaseJob
		renewCtx, release := context.WithTimeout(ctx, geminiWebHistoryDBTimeout)
		err := s.jobs.RenewGeminiWebHistoryCleanup(renewCtx, &copy, geminiWebHistoryLeaseDuration)
		release()
		if err != nil {
			cancel(ErrGeminiWebHistoryLeaseLost)
			return false
		}
		return ctx.Err() == nil
	}
	var heartbeat sync.WaitGroup
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		ticker := time.NewTicker(s.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !renew() {
					return
				}
			}
		}
	}()
	defer func() { cancel(nil); heartbeat.Wait() }()
	if !renew() {
		return
	}
	outcome := s.executeJob(ctx, job, renew)
	if ctx.Err() != nil || outcome.code == "lease_lost" || !renew() {
		return
	}
	writeCtx, release := context.WithTimeout(ctx, geminiWebHistoryDBTimeout)
	defer release()
	var err error
	status := outcome.code
	switch {
	case outcome.code == "":
		status = "completed"
		err = s.jobs.FinishGeminiWebHistoryCleanup(writeCtx, job)
	case outcome.cancel:
		err = s.jobs.CancelGeminiWebHistoryCleanup(writeCtx, job, outcome.code)
	default:
		err = s.jobs.RetryGeminiWebHistoryCleanup(writeCtx, job, outcome.code, s.now().Add(geminiWebHistoryRetryDelay(job.Attempts)))
	}
	if err != nil {
		status = "job_write_failed"
	}
	slog.Info("gemini_web_history_cleanup", "account_id", job.AccountID, "job_id", job.ID, "count", outcome.count, "status", status)
}

func geminiWebHistoryRetryDelay(attempts int) time.Duration {
	return min(15*time.Second*time.Duration(1<<min(max(attempts-1, 0), 5)), 5*time.Minute)
}

func geminiWebHistoryMatchingWindows(job, fresh []GeminiWebCleanupWindow, now time.Time) []GeminiWebCleanupWindow {
	var matches []GeminiWebCleanupWindow
	for _, want := range job {
		for _, got := range fresh {
			if want.ID == got.ID && want.ResetAt.Equal(got.ResetAt) && got.ResetAt.After(now) {
				matches = append(matches, got)
				break
			}
		}
	}
	return matches
}

func (s *GeminiWebHistoryCleanupService) loadAccount(ctx context.Context, job *GeminiWebHistoryCleanupJob, windows []GeminiWebCleanupWindow) (*Account, geminiWebHistoryOutcome) {
	if ctx.Err() != nil {
		return nil, geminiWebHistoryOutcome{code: "shutdown"}
	}
	readCtx, cancel := context.WithTimeout(ctx, geminiWebHistoryDBTimeout)
	account, err := s.accountRepo.GetByID(readCtx, job.AccountID)
	cancel()
	if errors.Is(err, ErrAccountNotFound) || (err == nil && !geminiWebHistoryAccountActive(account, s.now())) {
		return nil, geminiWebHistoryOutcome{code: "account_unavailable", cancel: true}
	}
	if err != nil {
		return nil, geminiWebHistoryOutcome{code: "internal_error"}
	}
	identity := geminiWebHistoryIdentity(account)
	if identity == "" || identity != job.Identity {
		return nil, geminiWebHistoryOutcome{code: "credential_changed", cancel: true}
	}
	for _, window := range windows {
		if window.ResetAt.After(s.now()) {
			return account, geminiWebHistoryOutcome{}
		}
	}
	return nil, geminiWebHistoryOutcome{code: "quota_window_changed", cancel: true}
}

func (s *GeminiWebHistoryCleanupService) executeJob(ctx context.Context, job *GeminiWebHistoryCleanupJob, renew func() bool) geminiWebHistoryOutcome {
	var account *Account
	for {
		var outcome geminiWebHistoryOutcome
		account, outcome = s.loadAccount(ctx, job, job.Windows)
		if outcome.code != "" {
			return outcome
		}
		checkCtx, cancel := context.WithTimeout(ctx, geminiWebHistoryDBTimeout)
		active, err := s.jobs.GeminiWebHistoryHasActiveRequests(checkCtx, job)
		cancel()
		if err != nil {
			if errors.Is(err, ErrGeminiWebHistoryLeaseLost) {
				return geminiWebHistoryOutcome{code: "lease_lost"}
			}
			return geminiWebHistoryOutcome{code: "active_requests"}
		}
		if !active {
			break
		}
		timer := time.NewTimer(s.drainInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return geminiWebHistoryOutcome{code: "shutdown"}
		case <-timer.C:
		}
	}
	// Refresh after even a long drain wait. A cached quota observation must
	// never authorize deletion, and a recovered/changed cycle cancels the job.
	quota, err := s.quotaRead(ctx, account)
	if err != nil {
		return geminiWebHistoryOutcome{code: "quota_unavailable"}
	}
	fresh := geminiWebCleanupWindows(quota, s.now())
	if len(fresh) == 0 {
		// Missing usage/reset metadata is not evidence of recovery. Keep the
		// original authorization pending until a complete read or cycle expiry.
		allRecovered := !quota.ObservedAt.IsZero() && s.now().Sub(quota.ObservedAt) <= 2*time.Minute && !quota.ObservedAt.After(s.now().Add(5*time.Second))
		for _, want := range job.Windows {
			if !want.ResetAt.After(s.now()) {
				continue
			}
			recovered := false
			for _, w := range quota.Windows {
				if w.ID == want.ID && w.HasPercent && w.UsedPercent >= 0 && w.UsedPercent < 100 && w.RemainingPercent > 0 && w.RemainingPercent <= 100 {
					recovered = true
				}
			}
			allRecovered = allRecovered && recovered
		}
		if allRecovered {
			return geminiWebHistoryOutcome{code: "quota_recovered", cancel: true}
		}
		return geminiWebHistoryOutcome{code: "quota_unavailable"}
	}
	windows := geminiWebHistoryMatchingWindows(job.Windows, fresh, s.now())
	if len(windows) == 0 {
		return geminiWebHistoryOutcome{code: "quota_unavailable"}
	}
	if ctx.Err() != nil {
		return geminiWebHistoryOutcome{code: "shutdown"}
	}
	ids, err := s.listConversations(ctx, account)
	if err != nil {
		return geminiWebHistoryOutcome{code: "history_list_failed"}
	}
	if !job.SnapshotReady {
		if !renew() {
			return geminiWebHistoryOutcome{code: "lease_lost"}
		}
		saveCtx, cancel := context.WithTimeout(ctx, geminiWebHistoryDBTimeout)
		err = s.jobs.SaveGeminiWebHistorySnapshot(saveCtx, job, ids)
		cancel()
		if err != nil {
			if errors.Is(err, ErrGeminiWebHistoryLeaseLost) {
				return geminiWebHistoryOutcome{code: "lease_lost"}
			}
			return geminiWebHistoryOutcome{code: "internal_error"}
		}
		job.Snapshot = append([]string{}, ids...)
		job.SnapshotReady = true
	}
	if !geminiWebHistoryValidIDs(job.Snapshot) {
		return geminiWebHistoryOutcome{code: "internal_error"}
	}
	present := make(map[string]bool, len(ids))
	for _, id := range ids {
		present[id] = true
	}
	deleted := 0
	for _, id := range job.Snapshot {
		if !present[id] {
			continue
		}
		var outcome geminiWebHistoryOutcome
		account, outcome = s.loadAccount(ctx, job, windows)
		if outcome.code != "" {
			return outcome
		}
		if s.now().Sub(quota.ObservedAt) > time.Minute {
			quota, err = s.quotaRead(ctx, account)
			if err != nil {
				return geminiWebHistoryOutcome{code: "quota_unavailable", count: deleted}
			}
			windows = geminiWebHistoryMatchingWindows(job.Windows, geminiWebCleanupWindows(quota, s.now()), s.now())
			if len(windows) == 0 {
				return geminiWebHistoryOutcome{code: "quota_unavailable", count: deleted}
			}
		}
		if !renew() {
			return geminiWebHistoryOutcome{code: "lease_lost"}
		}
		if err := s.deleteConversation(ctx, account, id); err != nil {
			// Google may commit a deletion before a timeout or an unfamiliar
			// acknowledgement. Only a complete fresh inventory can prove it.
			if !renew() {
				return geminiWebHistoryOutcome{code: "lease_lost", count: deleted}
			}
			current, listErr := s.listConversations(ctx, account)
			if listErr != nil {
				return geminiWebHistoryOutcome{code: "history_delete_failed", count: deleted}
			}
			for _, remaining := range current {
				if remaining == id {
					return geminiWebHistoryOutcome{code: "history_delete_failed", count: deleted}
				}
			}
		}
		deleted++
	}
	var outcome geminiWebHistoryOutcome
	account, outcome = s.loadAccount(ctx, job, windows)
	if outcome.code != "" {
		return outcome
	}
	ids, err = s.listConversations(ctx, account)
	if err != nil {
		return geminiWebHistoryOutcome{code: "history_verify_failed", count: deleted}
	}
	targets := make(map[string]bool, len(job.Snapshot))
	for _, id := range job.Snapshot {
		targets[id] = true
	}
	for _, id := range ids {
		if targets[id] {
			return geminiWebHistoryOutcome{code: "history_verify_failed", count: deleted}
		}
	}
	if _, outcome = s.loadAccount(ctx, job, windows); outcome.code != "" {
		return outcome
	}
	return geminiWebHistoryOutcome{count: deleted}
}

func geminiWebHistoryValidIDs(ids []string) bool {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || strings.TrimSpace(id) != id || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func (s *GeminiWebHistoryCleanupService) historyRequest(account *Account) (geminiweb.Credential, geminiweb.Doer) {
	cred := geminiWebCacheCredential(account, account.GetCredential("gemini_web_cookie"))
	cred.BaseURL, cred.UserAgent = "https://gemini.google.com", account.GetCredential("user_agent")
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	return cred, func(req *http.Request) (*http.Response, error) {
		return s.compat.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
}

func (s *GeminiWebHistoryCleanupService) persistHistoryCookie(ctx context.Context, account *Account, cred geminiweb.Credential, updated string) {
	if ctx.Err() != nil {
		return
	}
	if live := s.client.CurrentCookie(cred); live != "" {
		updated = live
	}
	if s.cookiePersist != nil {
		s.cookiePersist(ctx, account, cred.Cookie, updated)
	}
}

func (s *GeminiWebHistoryCleanupService) listConversations(ctx context.Context, account *Account) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Individual list RPCs are bounded in the client; a complete paginated
	// inventory may require many of them while the worker keeps its lease.
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cred, do := s.historyRequest(account)
	result, err := s.client.ListConversations(opCtx, cred, do)
	s.persistHistoryCookie(ctx, account, cred, result.UpdatedCookie)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !geminiWebHistoryValidIDs(result.ConversationIDs) {
		return nil, errors.New("invalid history listing")
	}
	return result.ConversationIDs, nil
}

func (s *GeminiWebHistoryCleanupService) deleteConversation(ctx context.Context, account *Account, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, geminiWebHistoryHTTPTimeout)
	defer cancel()
	cred, do := s.historyRequest(account)
	updated, err := s.client.DeleteConversation(opCtx, cred, id, do)
	s.persistHistoryCookie(ctx, account, cred, updated)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

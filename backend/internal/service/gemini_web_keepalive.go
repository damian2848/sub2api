package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

// GeminiWebCookieKeepaliveService mirrors cliproxyapi's browser-cookie
// conductor. Gemini Web cookies have no explicit expiry field; Google rotates
// __Secure-1PSIDTS periodically, so active accounts are renewed in the
// background even when no request has used them recently.
type GeminiWebCookieKeepaliveService struct {
	accountRepo  AccountRepository
	httpUpstream HTTPUpstream
	client       *geminiweb.Client
	interval     time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	start  sync.Once
	once   sync.Once
}

const (
	geminiWebKeepaliveInterval = 10 * time.Minute
	geminiWebKeepaliveTimeout  = 30 * time.Second
	geminiWebCookieLastRefresh = "gemini_web_cookie_last_refresh"
)

var geminiWebCredentialPersistLocks sync.Map // map[int64]*sync.Mutex

func geminiWebCredentialPersistLock(accountID int64) *sync.Mutex {
	actual, _ := geminiWebCredentialPersistLocks.LoadOrStore(accountID, &sync.Mutex{})
	lock, ok := actual.(*sync.Mutex)
	if !ok {
		panic("gemini web credential persist lock has unexpected type")
	}
	return lock
}

func NewGeminiWebCookieKeepaliveService(accountRepo AccountRepository, httpUpstream HTTPUpstream, clients ...*geminiweb.Client) *GeminiWebCookieKeepaliveService {
	ctx, cancel := context.WithCancel(context.Background())
	client := geminiweb.NewClient()
	if len(clients) > 0 && clients[0] != nil {
		client = clients[0]
	}
	return &GeminiWebCookieKeepaliveService{
		accountRepo: accountRepo, httpUpstream: httpUpstream,
		client: client, interval: geminiWebKeepaliveInterval,
		ctx: ctx, cancel: cancel,
	}
}

// Start performs one scan immediately, then renews active Gemini Web accounts
// every ten minutes. Errors are account-scoped and do not stop the worker.
func (s *GeminiWebCookieKeepaliveService) Start() {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return
	}
	s.start.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runOnce(s.ctx)
			ticker := time.NewTicker(s.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					s.runOnce(s.ctx)
				case <-s.ctx.Done():
					return
				}
			}
		}()
	})
}

func (s *GeminiWebCookieKeepaliveService) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() { s.cancel() })
	s.wg.Wait()
}

func (s *GeminiWebCookieKeepaliveService) runOnce(ctx context.Context) {
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformGemini)
	if err != nil {
		return
	}
	for i := range accounts {
		account := &accounts[i]
		if !account.IsGeminiWeb() {
			continue
		}
		if account.RateLimitResetAt != nil && time.Now().Before(*account.RateLimitResetAt) {
			continue
		}
		if account.TempUnschedulableUntil != nil && time.Now().Before(*account.TempUnschedulableUntil) {
			continue
		}
		if refreshedAt := account.GetCredentialAsTime(geminiWebCookieLastRefresh); refreshedAt != nil && time.Since(*refreshedAt) < s.interval {
			continue
		}
		// A cancelled worker should stop before starting a new network request;
		// an in-flight request is bounded by geminiWebKeepaliveTimeout.
		select {
		case <-ctx.Done():
			return
		default:
		}
		s.refreshAccount(ctx, account)
	}
}

func (s *GeminiWebCookieKeepaliveService) refreshAccount(parent context.Context, account *Account) {
	if account == nil {
		return
	}
	cookie := strings.TrimSpace(account.GetCredential("gemini_web_cookie"))
	if cookie == "" {
		return
	}
	requestCtx, cancel := context.WithTimeout(parent, geminiWebKeepaliveTimeout)
	defer cancel()
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
		return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
	updatedCookie, err := s.client.RefreshCookies(requestCtx, cred, do)
	if err == nil {
		persistLock := geminiWebCredentialPersistLock(account.ID)
		persistLock.Lock()
		defer persistLock.Unlock()
		// Another request may have rotated the same live session while the
		// response was being handled. Persist the newest cached generation.
		if live := s.client.CurrentCookie(cred); live != "" {
			updatedCookie = live
		}
		updated := shallowCopyMap(account.Credentials)
		if updatedCookie != "" {
			updated["gemini_web_cookie"] = updatedCookie
		}
		updated[geminiWebCookieLastRefresh] = time.Now().UTC().Format(time.RFC3339Nano)
		// Keep a successfully rotated cookie in memory even if a transient DB
		// error occurs; the next request can still use the live credential.
		persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
		defer persistCancel()
		if persistErr := persistAccountCredentials(persistCtx, s.accountRepo, account, updated); persistErr != nil {
			account.Credentials = updated
		}
	}
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, geminiweb.ErrInvalidCredential):
		until := time.Now().Add(10 * time.Minute)
		_ = s.accountRepo.SetTempUnschedulable(parent, account.ID, until, "Gemini Web cookie rotation rejected")
	case errors.Is(err, geminiweb.ErrRateLimited):
		_ = s.accountRepo.SetRateLimited(parent, account.ID, time.Now().Add(time.Minute))
	}
}

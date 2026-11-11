//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// scriptedSlotCache answers account slot acquires from a per-account script;
// once a script runs out the account stays busy.
type scriptedSlotCache struct {
	fakeConcurrencyCache
	mu       sync.Mutex
	script   map[int64][]bool
	attempts map[int64]int
}

func (c *scriptedSlotCache) AcquireAccountSlot(_ context.Context, accountID int64, _ int, _ string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts[accountID]++
	steps := c.script[accountID]
	if len(steps) == 0 {
		return false, nil
	}
	c.script[accountID] = steps[1:]
	return steps[0], nil
}

func (c *scriptedSlotCache) attemptsFor(accountID int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts[accountID]
}

// queueFullHelperCache reports every account queue as full and counts the
// increments, so a test can tell whether a second queue was joined.
type queueFullHelperCache struct {
	fakeConcurrencyCache
	mu         sync.Mutex
	increments map[int64]int
}

func (c *queueFullHelperCache) IncrementAccountWaitCount(_ context.Context, accountID int64, _ int) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.increments[accountID]++
	return false, nil
}

type recordingUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	accounts []int64
}

func (u *recordingUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accounts = append(u.accounts, accountID)
	u.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"not_found_error","message":"stop here"}}`)),
	}, nil
}

func (u *recordingUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func (u *recordingUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accounts...)
}

const (
	queueFullPrimaryAccount   = int64(1301)
	queueFullSecondaryAccount = int64(1302)
)

func queueFullAccount(id int64, priority int, groupID int64, accountType string, extra map[string]any) *service.Account {
	credentials := map[string]any{"api_key": "sk-test", "base_url": "https://upstream.invalid"}
	if accountType == service.AccountTypeOAuth {
		credentials = map[string]any{"access_token": "tok"}
	}
	return &service.Account{
		ID:            id,
		Name:          "queue-full",
		Platform:      service.PlatformAnthropic,
		Type:          accountType,
		Credentials:   credentials,
		Extra:         extra,
		Concurrency:   1,
		Priority:      priority,
		Status:        service.StatusActive,
		Schedulable:   true,
		AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}},
	}
}

func newQueueFullReselectHandler(t *testing.T, group *service.Group, accounts []*service.Account, gatewayCache *scriptedSlotCache, helperCache *queueFullHelperCache, sessions service.SessionLimitCache, upstream service.HTTPUpstream, reselect bool) (*GatewayHandler, func()) {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Gateway.Scheduling.FallbackProbeHardSlots = true
	cfg.Gateway.Scheduling.QueueFullReselect = reselect
	cfg.Gateway.Scheduling.FallbackMaxWaiting = 1
	cfg.Gateway.Scheduling.FallbackWaitTimeout = 50 * time.Millisecond
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 1
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 50 * time.Millisecond

	schedulerSnapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
	gwSvc := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil,
		nil, // cache (disable sticky)
		cfg,
		schedulerSnapshot,
		service.NewConcurrencyService(gatewayCache),
		nil, nil, nil, nil,
		upstream,
		nil, nil,
		sessions,
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	h := &GatewayHandler{
		gatewayService:           gwSvc,
		billingCacheService:      billingCacheSvc,
		concurrencyHelper:        NewConcurrencyHelper(service.NewConcurrencyService(helperCache), SSEPingFormatClaude, 0),
		maxAccountSwitches:       1,
		maxAccountSwitchesGemini: 1,
	}
	return h, billingCacheSvc.Stop
}

func serveQueueFullMessages(t *testing.T, h *GatewayHandler, group *service.Group) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"queue full reselect"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	groupID := group.ID
	apiKey := &service.APIKey{
		ID: 3301, UserID: 4301, GroupID: &groupID, Status: service.StatusActive,
		User:  &service.User{ID: 4301, Concurrency: 10, Balance: 100},
		Group: group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	h.Messages(c)
	return rec
}

// The primary account's queue is full, but a slot on the next account frees up
// by the time the handler reselects: the request moves there instead of 429.
func TestGatewayHandlerMessages_QueueFullReselectsAccountWithFreeSlot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 2301, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	accounts := []*service.Account{
		queueFullAccount(queueFullPrimaryAccount, 1, group.ID, service.AccountTypeAPIKey, nil),
		queueFullAccount(queueFullSecondaryAccount, 2, group.ID, service.AccountTypeAPIKey, nil),
	}
	gatewayCache := &scriptedSlotCache{
		script:   map[int64][]bool{queueFullSecondaryAccount: {false, true}},
		attempts: map[int64]int{},
	}
	helperCache := &queueFullHelperCache{increments: map[int64]int{}}
	upstream := &recordingUpstream{}
	h, cleanup := newQueueFullReselectHandler(t, group, accounts, gatewayCache, helperCache, newRecordingSessionLimitCache(), upstream, true)
	defer cleanup()

	rec := serveQueueFullMessages(t, h, group)

	require.NotEqual(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.Equal(t, []int64{queueFullSecondaryAccount}, upstream.calls(), "the reselected account serves the request")
	require.Equal(t, map[int64]int{queueFullPrimaryAccount: 1}, helperCache.increments, "only the primary queue was tried")
}

// When the reselection only yields another wait plan, the request is not parked
// on the next account's queue: it keeps the 429 and gives back the session the
// discarded wait plan registered.
func TestGatewayHandlerMessages_QueueFullDoesNotQueueOnNextAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 2302, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	limited := map[string]any{"max_sessions": 2}
	accounts := []*service.Account{
		queueFullAccount(queueFullPrimaryAccount, 1, group.ID, service.AccountTypeOAuth, limited),
		queueFullAccount(queueFullSecondaryAccount, 2, group.ID, service.AccountTypeOAuth, limited),
	}
	gatewayCache := &scriptedSlotCache{script: map[int64][]bool{}, attempts: map[int64]int{}}
	helperCache := &queueFullHelperCache{increments: map[int64]int{}}
	sessions := newRecordingSessionLimitCache()
	upstream := &recordingUpstream{}
	h, cleanup := newQueueFullReselectHandler(t, group, accounts, gatewayCache, helperCache, sessions, upstream, true)
	defer cleanup()

	rec := serveQueueFullMessages(t, h, group)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Contains(t, rec.Body.String(), gatewayQueueFullCode)
	require.Empty(t, upstream.calls())
	require.Equal(t, map[int64]int{queueFullPrimaryAccount: 1}, helperCache.increments, "the next account's queue must not be joined")
	for _, id := range []int64{queueFullPrimaryAccount, queueFullSecondaryAccount} {
		registered, unregistered := sessions.snapshot(id)
		require.Len(t, registered, 1, "account %d", id)
		require.Equal(t, registered, unregistered, "account %d session must be released", id)
	}
}

func TestGatewayHandlerMessages_QueueFullReselectDisabledKeeps429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 2303, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	accounts := []*service.Account{
		queueFullAccount(queueFullPrimaryAccount, 1, group.ID, service.AccountTypeAPIKey, nil),
		queueFullAccount(queueFullSecondaryAccount, 2, group.ID, service.AccountTypeAPIKey, nil),
	}
	gatewayCache := &scriptedSlotCache{
		script:   map[int64][]bool{queueFullSecondaryAccount: {false, true}},
		attempts: map[int64]int{},
	}
	helperCache := &queueFullHelperCache{increments: map[int64]int{}}
	upstream := &recordingUpstream{}
	h, cleanup := newQueueFullReselectHandler(t, group, accounts, gatewayCache, helperCache, newRecordingSessionLimitCache(), upstream, false)
	defer cleanup()

	rec := serveQueueFullMessages(t, h, group)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Contains(t, rec.Body.String(), gatewayQueueFullCode)
	require.Empty(t, upstream.calls())
	require.Equal(t, 1, gatewayCache.attemptsFor(queueFullSecondaryAccount), "no second selection when queue_full_reselect is off")
}

// Gemini accounts on /v1/messages take the same one-shot reselection: the
// second account is probed again, but its queue is never joined.
func TestGatewayHandlerMessages_GeminiQueueFullReselectsWithoutQueueing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 2304, Hydrated: true, Platform: service.PlatformGemini, Status: service.StatusActive}
	newGemini := func(id int64, priority int) *service.Account {
		return &service.Account{
			ID: id, Name: "gemini-busy", Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "g-test"},
			Concurrency: 1, Priority: priority, Status: service.StatusActive, Schedulable: true,
			AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: group.ID}},
		}
	}
	accounts := []*service.Account{newGemini(queueFullPrimaryAccount, 1), newGemini(queueFullSecondaryAccount, 2)}
	gatewayCache := &scriptedSlotCache{script: map[int64][]bool{}, attempts: map[int64]int{}}
	helperCache := &queueFullHelperCache{increments: map[int64]int{}}
	h, cleanup := newQueueFullReselectHandler(t, group, accounts, gatewayCache, helperCache, newRecordingSessionLimitCache(), &recordingUpstream{}, true)
	defer cleanup()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := `{"model":"gemini-2.5-pro","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	groupID := group.ID
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 3304, UserID: 4304, GroupID: &groupID, Status: service.StatusActive, Group: group, User: &service.User{ID: 4304, Concurrency: 10, Balance: 100}})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 4304, Concurrency: 10})
	h.Messages(c)

	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), gatewayQueueFullCode)
	require.Equal(t, map[int64]int{queueFullPrimaryAccount: 1}, helperCache.increments)
	require.Equal(t, 2, gatewayCache.attemptsFor(queueFullSecondaryAccount), "the reselection probes the next account once more")
}

// The warmup intercept ran only against the primary account. When the queue-full
// reselection moves the request to an account that intercepts warmups, the
// warmup is answered locally there too instead of being sent upstream.
func TestGatewayHandlerMessages_QueueFullReselectReappliesWarmupIntercept(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 2305, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	secondary := queueFullAccount(queueFullSecondaryAccount, 2, group.ID, service.AccountTypeAPIKey, nil)
	secondary.Credentials["intercept_warmup_requests"] = true
	accounts := []*service.Account{
		queueFullAccount(queueFullPrimaryAccount, 1, group.ID, service.AccountTypeAPIKey, nil),
		secondary,
	}
	gatewayCache := &scriptedSlotCache{
		script:   map[int64][]bool{queueFullSecondaryAccount: {false, true}},
		attempts: map[int64]int{},
	}
	helperCache := &queueFullHelperCache{increments: map[int64]int{}}
	upstream := &recordingUpstream{}
	h, cleanup := newQueueFullReselectHandler(t, group, accounts, gatewayCache, helperCache, newRecordingSessionLimitCache(), upstream, true)
	defer cleanup()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"Warmup"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	groupID := group.ID
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 3305, UserID: 4305, GroupID: &groupID, Status: service.StatusActive, Group: group, User: &service.User{ID: 4305, Concurrency: 10, Balance: 100}})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 4305, Concurrency: 10})
	h.Messages(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, upstream.calls(), "the warmup must not reach the reselected account's upstream")
	require.Equal(t, map[int64]int{queueFullPrimaryAccount: 1}, helperCache.increments)
}

// A discarded wait plan normally gives back the session it registered, but not
// when the session was already bound to that account: UnregisterSession is not
// reference counted, so it would drop a registration other in-flight requests
// of the same session still rely on.
func TestReselectAfterAccountQueueFull_KeepsSessionOnBoundAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limited := map[string]any{"max_sessions": 2}
	for _, tc := range []struct {
		name        string
		bound       int64
		wantRelease bool
	}{
		{name: "unbound session is released", bound: 0, wantRelease: true},
		{name: "bound session is kept", bound: queueFullSecondaryAccount, wantRelease: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &service.Group{ID: 2306, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			accounts := []*service.Account{
				queueFullAccount(queueFullPrimaryAccount, 1, group.ID, service.AccountTypeOAuth, limited),
				queueFullAccount(queueFullSecondaryAccount, 2, group.ID, service.AccountTypeOAuth, limited),
			}
			gatewayCache := &scriptedSlotCache{script: map[int64][]bool{}, attempts: map[int64]int{}}
			sessions := newRecordingSessionLimitCache()
			h, cleanup := newQueueFullReselectHandler(t, group, accounts, gatewayCache, &queueFullHelperCache{increments: map[int64]int{}}, sessions, &recordingUpstream{}, true)
			defer cleanup()

			groupID := group.ID
			ctx := context.WithValue(context.Background(), ctxkey.Group, group)
			got := h.reselectAfterAccountQueueFull(ctx, &groupID, "sess-bound", "claude-sonnet-4-5", nil, queueFullPrimaryAccount, tc.bound, "", 0)

			require.Nil(t, got, "a wait plan on the next account is never used")
			registered, unregistered := sessions.snapshot(queueFullSecondaryAccount)
			require.Len(t, registered, 1)
			if tc.wantRelease {
				require.Equal(t, registered, unregistered)
			} else {
				require.Empty(t, unregistered)
			}
		})
	}
}

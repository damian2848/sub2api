//go:build unit

package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// recordingSessionLimitCache records session registrations so tests can check
// that a request which never reached upstream gives its session back.
type recordingSessionLimitCache struct {
	mu           sync.Mutex
	registered   map[int64][]string
	unregistered map[int64][]string
}

func newRecordingSessionLimitCache() *recordingSessionLimitCache {
	return &recordingSessionLimitCache{registered: map[int64][]string{}, unregistered: map[int64][]string{}}
}

func (c *recordingSessionLimitCache) RegisterSession(_ context.Context, accountID int64, sessionUUID string, _ int, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registered[accountID] = append(c.registered[accountID], sessionUUID)
	return true, nil
}
func (c *recordingSessionLimitCache) RefreshSession(context.Context, int64, string, time.Duration) error {
	return nil
}
func (c *recordingSessionLimitCache) UnregisterSession(_ context.Context, accountID int64, sessionUUID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unregistered[accountID] = append(c.unregistered[accountID], sessionUUID)
	return nil
}
func (c *recordingSessionLimitCache) GetActiveSessionCount(context.Context, int64) (int, error) {
	return 0, nil
}
func (c *recordingSessionLimitCache) GetActiveSessionCountBatch(context.Context, []int64, map[int64]time.Duration) (map[int64]int, error) {
	return map[int64]int{}, nil
}
func (c *recordingSessionLimitCache) IsSessionActive(context.Context, int64, string) (bool, error) {
	return false, nil
}
func (c *recordingSessionLimitCache) GetWindowCost(context.Context, int64) (float64, bool, error) {
	return 0, false, nil
}
func (c *recordingSessionLimitCache) SetWindowCost(context.Context, int64, float64) error { return nil }
func (c *recordingSessionLimitCache) GetWindowCostBatch(context.Context, []int64) (map[int64]float64, error) {
	return map[int64]float64{}, nil
}

func (c *recordingSessionLimitCache) snapshot(accountID int64) (registered, unregistered []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.registered[accountID]...), append([]string(nil), c.unregistered[accountID]...)
}

// slotConcurrencyCache controls whether account slots and account queue
// entries can be taken.
type slotConcurrencyCache struct {
	fakeConcurrencyCache
	acquireAccount bool
	canWait        bool
	mu             sync.Mutex
	waitIncrements int
	waitDecrements int
}

func (c *slotConcurrencyCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	return c.acquireAccount, nil
}

func (c *slotConcurrencyCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.canWait {
		c.waitIncrements++
	}
	return c.canWait, nil
}

func (c *slotConcurrencyCache) DecrementAccountWaitCount(context.Context, int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waitDecrements++
	return nil
}

func (c *slotConcurrencyCache) waitCounts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waitIncrements, c.waitDecrements
}

func newSessionReleaseTestHandler(t *testing.T, group *service.Group, account *service.Account, gatewayCache, helperCache service.ConcurrencyCache, sessions service.SessionLimitCache) (*GatewayHandler, func()) {
	t.Helper()

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Gateway.Scheduling.FallbackProbeHardSlots = true
	cfg.Gateway.Scheduling.FallbackMaxWaiting = 1
	cfg.Gateway.Scheduling.FallbackWaitTimeout = 50 * time.Millisecond
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 1
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 50 * time.Millisecond

	schedulerSnapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
	gwSvc := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil,
		nil, // cache (disable sticky)
		cfg,
		schedulerSnapshot,
		service.NewConcurrencyService(gatewayCache),
		nil, nil, nil, nil, nil, nil, nil,
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

func runSessionReleaseMessages(t *testing.T, h *GatewayHandler, group *service.Group, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	groupID := group.ID
	apiKey := &service.APIKey{
		ID: 3101, UserID: 4101, GroupID: &groupID, Status: service.StatusActive,
		User:  &service.User{ID: 4101, Concurrency: 10, Balance: 100},
		Group: group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	h.Messages(c)
	return rec
}

func sessionLimitedOAuthAccount(groupID int64, credentials map[string]any) *service.Account {
	accountID := int64(1101)
	return &service.Account{
		ID:            accountID,
		Name:          "oauth-limited",
		Platform:      service.PlatformAnthropic,
		Type:          service.AccountTypeOAuth,
		Credentials:   credentials,
		Extra:         map[string]any{"max_sessions": 2},
		Concurrency:   1,
		Priority:      1,
		Status:        service.StatusActive,
		Schedulable:   true,
		AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}
}

const sessionReleaseBody = `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hello session release"}]}]}`

func TestGatewayHandlerMessages_QueueFullReleasesRegisteredSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 2101, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := sessionLimitedOAuthAccount(group.ID, map[string]any{"access_token": "tok"})
	sessions := newRecordingSessionLimitCache()
	h, cleanup := newSessionReleaseTestHandler(t, group, account,
		&slotConcurrencyCache{acquireAccount: false},
		&slotConcurrencyCache{acquireAccount: false, canWait: false},
		sessions,
	)
	defer cleanup()

	rec := runSessionReleaseMessages(t, h, group, sessionReleaseBody)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Contains(t, rec.Body.String(), gatewayQueueFullCode)
	registered, unregistered := sessions.snapshot(account.ID)
	require.NotEmpty(t, registered, "the wait plan registers the session before the handler queues")
	require.Equal(t, registered[:1], unregistered, "a queue-full 429 must give the session back at once")
}

func TestGatewayHandlerMessages_WaitTimeoutReleasesRegisteredSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 2102, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := sessionLimitedOAuthAccount(group.ID, map[string]any{"access_token": "tok"})
	sessions := newRecordingSessionLimitCache()
	helperCache := &slotConcurrencyCache{acquireAccount: false, canWait: true}
	h, cleanup := newSessionReleaseTestHandler(t, group, account,
		&slotConcurrencyCache{acquireAccount: false},
		helperCache,
		sessions,
	)
	defer cleanup()

	rec := runSessionReleaseMessages(t, h, group, sessionReleaseBody)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	registered, unregistered := sessions.snapshot(account.ID)
	require.NotEmpty(t, registered)
	require.Equal(t, registered[:1], unregistered, "a wait timeout must give the session back at once")
	inc, dec := helperCache.waitCounts()
	require.Equal(t, 1, inc)
	require.Equal(t, 1, dec)
}

// The warmup intercept answers locally right after selection; it keeps the
// existing behaviour and does not release the session.
func TestGatewayHandlerMessages_WarmupInterceptKeepsSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 2103, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := sessionLimitedOAuthAccount(group.ID, map[string]any{"access_token": "tok", "intercept_warmup_requests": true})
	sessions := newRecordingSessionLimitCache()
	h, cleanup := newSessionReleaseTestHandler(t, group, account,
		&slotConcurrencyCache{acquireAccount: true},
		&slotConcurrencyCache{acquireAccount: true, canWait: true},
		sessions,
	)
	defer cleanup()

	rec := runSessionReleaseMessages(t, h, group, `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"Warmup"}]}]}`)

	require.Equal(t, http.StatusOK, rec.Code)
	registered, unregistered := sessions.snapshot(account.ID)
	require.NotEmpty(t, registered)
	require.Empty(t, unregistered)
}

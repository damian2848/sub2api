//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type geminiWaitStickyCache struct {
	service.GatewayCache
	bound  int64
	writes []int64
}

func (s *geminiWaitStickyCache) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return s.bound, nil
}
func (s *geminiWaitStickyCache) SetSessionAccountID(_ context.Context, _ int64, _ string, id int64, _ time.Duration) error {
	s.bound = id
	s.writes = append(s.writes, id)
	return nil
}
func (s *geminiWaitStickyCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}
func (s *geminiWaitStickyCache) DeleteSessionAccountID(context.Context, int64, string) error {
	s.bound = 0
	return nil
}

type geminiWaitUpstream struct {
	service.HTTPUpstream
	do func(*http.Request, int64) (*http.Response, error)
}

func (u *geminiWaitUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	return u.do(req, id)
}

func TestGeminiV1BetaModels_QueueReselectsWithoutSpeculativeStickyOrSignatureLeak(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var start time.Time
	groupID := int64(9400)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAntigravity, Status: service.StatusActive}
	accounts := []*service.Account{}
	for _, id := range []int64{1, 2} {
		accounts = append(accounts, &service.Account{ID: id, Platform: service.PlatformAntigravity, Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "test-key", "model_mapping": map[string]any{"gemini-2.5-flash": "gemini-2.5-flash"}},
			Status:      service.StatusActive, Schedulable: true, Concurrency: 1, Priority: int(id), AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}}})
	}
	cache := newGeminiWaitCache()
	cache.ConcurrencyCache = &fakeConcurrencyCache{}
	sticky := &geminiWaitStickyCache{bound: 1}
	probes := 0
	cache.acquire = func(_ context.Context, id int64) (bool, error) {
		if id == 2 {
			probes++
			require.Equal(t, int64(1), sticky.bound, "a probe must not move the binding before acquiring a slot")
			require.Empty(t, sticky.writes)
			return time.Since(start) >= 2*time.Second, nil
		}
		return false, nil
	}
	concurrency := service.NewConcurrencyService(cache)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{StickySessionMaxWaiting: 3, StickySessionWaitTimeout: 120 * time.Second, FallbackWaitTimeout: 30 * time.Second, FallbackMaxWaiting: 10, LoadBatchEnabled: false}
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
	gateway := service.NewGatewayService(nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, sticky, cfg, snapshot, concurrency,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h, cleanup := newTestGatewayHandler(t, group, accounts)
	defer cleanup()
	h.gatewayService = gateway
	h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, 0)
	upstreamCalls := 0
	upstream := &geminiWaitUpstream{do: func(req *http.Request, id int64) (*http.Response, error) {
		upstreamCalls++
		require.Equal(t, int64(2), id)
		require.Equal(t, int64(2), sticky.bound)
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.NotContains(t, string(body), "private-account-one-signature")
		_, hasDeadline := req.Context().Deadline()
		require.False(t, hasDeadline, "the queue deadline must not cancel upstream generation")
		time.Sleep(35 * time.Second)
		require.NoError(t, req.Context().Err())
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"text":"completed after queue budget"}]},"finishReason":"STOP"}]}`))}, nil
	}}
	h.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, upstream, nil, &config.Config{})
	apiKey := &service.APIKey{ID: 9401, UserID: 9402, GroupID: &groupID, Group: group, Status: service.StatusActive, User: &service.User{ID: 9402, Concurrency: 10, Balance: 100}}
	router := gin.New()
	router.POST("/v1beta/models/*modelAction", func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set("force_platform", service.PlatformAntigravity)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
		h.GeminiV1BetaModels(c)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", bytes.NewBufferString(`{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"thinking","thoughtSignature":"private-account-one-signature"}]},{"role":"user","parts":[{"text":"continue"}]}]}`))
	ctx := context.WithValue(req.Context(), ctxkey.Group, group)
	ctx = context.WithValue(ctx, ctxkey.ForcePlatform, service.PlatformAntigravity)
	req = req.WithContext(ctx)
	synctest.Test(t, func(t *testing.T) {
		start = time.Now()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "completed after queue budget")
		require.Equal(t, 1, upstreamCalls)
		require.Equal(t, 2, probes)
		require.Equal(t, 37*time.Second, time.Since(start))
		require.Equal(t, []int64{2}, sticky.writes)
		require.Equal(t, map[int64]int{1: 1}, cache.increments)
		require.Equal(t, cache.increments, cache.decrements)
		require.Equal(t, map[int64]int{2: 1}, cache.releases)
	})
}

type geminiWebWaitAccountRepo struct {
	service.AccountRepository
	accounts map[int64]*service.Account
}

func (r *geminiWebWaitAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	return r.accounts[id], nil
}

func TestGeminiWebAdmission_AllProtocolsReselectAndRejectCooledQueuedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []struct {
		name, route, path, body string
		call                    func(*GatewayHandler) gin.HandlerFunc
	}{
		{"native", "/v1beta/models/*modelAction", "/v1beta/models/gemini-4-argon:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, func(h *GatewayHandler) gin.HandlerFunc { return h.GeminiV1BetaModels }},
		{"chat", "/v1/chat/completions", "/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`, func(h *GatewayHandler) gin.HandlerFunc { return h.ChatCompletions }},
		{"messages", "/v1/messages", "/v1/messages", `{"model":"gemini-4-argon","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, func(h *GatewayHandler) gin.HandlerFunc { return h.Messages }},
	} {
		for _, scenario := range []string{"alternative", "cooled_while_waiting", "slow_failed_generation", "admission_rejections"} {
			t.Run(protocol.name+"/"+scenario, func(t *testing.T) {
				t.Setenv("DATA_DIR", t.TempDir())
				coolQueued, slowFailure := scenario == "cooled_while_waiting", scenario == "slow_failed_generation"
				admissionReject := scenario == "admission_rejections"
				groupID := int64(9500)
				group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformGemini, Status: service.StatusActive}
				accounts := []*service.Account{}
				repo := &geminiWebWaitAccountRepo{accounts: map[int64]*service.Account{}}
				ids := []int64{1, 2}
				if admissionReject {
					ids = append(ids, 3)
				}
				for _, id := range ids {
					a := &service.Account{ID: id, Platform: service.PlatformGemini, Type: service.AccountTypeGeminiWeb,
						Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=handler-wait-cookie", "model_mapping": map[string]any{"gemini-4-argon": "gemini-4-argon"}},
						Status:      service.StatusActive, Schedulable: true, Concurrency: 1, Priority: int(id), AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}}}
					accounts = append(accounts, a)
					copy := *a
					repo.accounts[id] = &copy
				}
				cache := newGeminiWaitCache()
				cache.ConcurrencyCache = &fakeConcurrencyCache{}
				sticky := &geminiWaitStickyCache{bound: 1}
				var start time.Time
				cooled := false
				cache.acquire = func(_ context.Context, id int64) (bool, error) {
					if admissionReject {
						return true, nil
					}
					if slowFailure {
						return id == 1 || time.Since(start) >= 37*time.Second, nil
					}
					if id == 1 && coolQueued && time.Since(start) >= 1500*time.Millisecond {
						until := time.Now().Add(time.Minute)
						repo.accounts[1].TempUnschedulableUntil = &until
						cooled = true
						return true, nil
					}
					if id == 2 {
						return (!coolQueued && time.Since(start) >= 2*time.Second) || cooled, nil
					}
					return false, nil
				}
				concurrency := service.NewConcurrencyService(cache)
				cfg := &config.Config{RunMode: config.RunModeSimple}
				cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{StickySessionMaxWaiting: 3, StickySessionWaitTimeout: 120 * time.Second, FallbackWaitTimeout: 30 * time.Second, FallbackMaxWaiting: 10, LoadBatchEnabled: false}
				snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
				gateway := service.NewGatewayService(repo, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, sticky, cfg, snapshot, concurrency,
					nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				h, cleanup := newTestGatewayHandler(t, group, accounts)
				defer cleanup()
				h.gatewayService = gateway
				h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, 0)
				forwarded := 0
				warmingHealth := admissionReject
				upstream := &geminiWaitUpstream{do: func(req *http.Request, id int64) (*http.Response, error) {
					if !slowFailure && !admissionReject {
						require.Equal(t, int64(2), id, "a cooled queued account must never reach upstream")
					}
					body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
					if admissionReject && id == 2 && strings.Contains(req.URL.Path, "batchexecute") {
						return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("quota exhausted"))}, nil
					}
					if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "StreamGenerate") {
						if warmingHealth {
							return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
						}
						if admissionReject {
							require.Equal(t, int64(3), id, "health and quota rejected accounts must never generate")
						}
						forwarded++
						body = geminiWebHandlerFrames("The available account answered.")
						if slowFailure && id == 1 {
							time.Sleep(35 * time.Second)
							body = geminiWebHandlerFrames("I seem to be encountering an error. Can I try something else for you?")
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
				}}
				h.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, upstream, nil, &config.Config{})
				apiKey := &service.APIKey{ID: 9501, UserID: 9502, GroupID: &groupID, Group: group, Status: service.StatusActive, User: &service.User{ID: 9502, Concurrency: 10, Balance: 100}}
				router := gin.New()
				router.POST(protocol.route, func(c *gin.Context) {
					c.Set(string(middleware.ContextKeyAPIKey), apiKey)
					c.Set("force_platform", service.PlatformGemini)
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
					protocol.call(h)(c)
				})
				req := httptest.NewRequest(http.MethodPost, protocol.path, strings.NewReader(protocol.body))
				ctx := context.WithValue(req.Context(), ctxkey.Group, group)
				req = req.WithContext(context.WithValue(ctx, ctxkey.ForcePlatform, service.PlatformGemini))
				synctest.Test(t, func(t *testing.T) {
					start = time.Now()
					if admissionReject {
						for range 3 {
							warmContext, _ := gin.CreateTestContext(httptest.NewRecorder())
							warmContext.Request = httptest.NewRequest(http.MethodPost, "/", nil)
							warmAccount := *accounts[0]
							_, err := h.geminiCompatService.ForwardNative(context.Background(), warmContext, &warmAccount, "gemini-4-argon", "generateContent", false, []byte(`{"contents":[{"role":"user","parts":[{"text":"warm"}]}]}`))
							require.Error(t, err)
						}
						warmingHealth = false
						h.maxAccountSwitchesGemini = 1
					}
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, req)
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					require.Contains(t, rec.Body.String(), "The available account answered.")
					require.Positive(t, forwarded)
					if admissionReject {
						require.Equal(t, 1, forwarded)
						require.Empty(t, cache.increments)
						require.Equal(t, map[int64]int{1: 1, 2: 1, 3: 1}, cache.releases)
					} else if slowFailure {
						require.Equal(t, 2, forwarded, "a slow failed generation must still allow failover")
						require.GreaterOrEqual(t, time.Since(start), 37*time.Second)
						require.Less(t, time.Since(start), 39*time.Second)
						require.Equal(t, map[int64]int{2: 1}, cache.increments)
					} else {
						require.Less(t, time.Since(start), 4*time.Second)
						require.Equal(t, map[int64]int{1: 1}, cache.increments, "chat must count its waiting reservation too")
					}
					require.Equal(t, cache.increments, cache.decrements)
					require.Equal(t, 1, cache.releases[2])
					if coolQueued {
						require.Equal(t, 1, cache.releases[1], "rejected admission must return the occupied slot")
					}
				})
			})
		}
	}
}

// Simulated upstream: gateway admission at 60 RPM and a full-minute burst.
type poolRecoveryLoadCache struct {
	fakeConcurrencyCache
	mu     sync.Mutex
	active map[int64]int
	peak   int
}

func (c *poolRecoveryLoadCache) AcquireAccountSlot(_ context.Context, id int64, limit int, _ string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[id] >= limit {
		return false, nil
	}
	c.active[id]++
	c.peak = max(c.peak, c.active[id])
	return true, nil
}
func (c *poolRecoveryLoadCache) ReleaseAccountSlot(_ context.Context, id int64, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active[id]--
	return nil
}

// Redis snapshots deserialize a fresh account for each read. The existing
// single-request fixture shares pointers and cannot model concurrent callers.
type poolRecoverySnapshotCache struct{ fakeSchedulerCache }

func (c *poolRecoverySnapshotCache) GetSnapshot(_ context.Context, _ service.SchedulerBucket) ([]*service.Account, bool, error) {
	data, err := json.Marshal(c.accounts)
	if err != nil {
		return nil, false, err
	}
	var accounts []*service.Account
	err = json.Unmarshal(data, &accounts)
	return accounts, err == nil, err
}
func (c *poolRecoverySnapshotCache) GetAccount(ctx context.Context, id int64) (*service.Account, error) {
	account, err := c.fakeSchedulerCache.GetAccount(ctx, id)
	if err != nil || account == nil {
		return account, err
	}
	data, err := json.Marshal(account)
	if err != nil {
		return nil, err
	}
	var fresh service.Account
	err = json.Unmarshal(data, &fresh)
	return &fresh, err
}

func TestAntigravityPoolRecoveryNative60RPM(t *testing.T) {
	t.Setenv("ANTIGRAVITY_POOL_RECOVERY_GROUP_IDS", "55")
	gin.SetMode(gin.TestMode)
	for _, scenario := range []struct {
		name      string
		count     int
		interval  time.Duration
		loadBatch bool
	}{
		{"steady_three_minutes", 180, time.Second, false}, {"sixty_request_burst", 60, 0, false},
		{"load_aware_steady", 180, time.Second, true}, {"load_aware_burst", 60, 0, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var start, resetAt time.Time
			groupID := int64(55)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAntigravity, Status: service.StatusActive}
			var accounts []*service.Account
			var persisted []service.Account
			for id := int64(1); id <= 11; id++ {
				a := &service.Account{ID: id, Platform: service.PlatformAntigravity, Type: service.AccountTypeAPIKey,
					Credentials: map[string]any{"api_key": "synthetic", "model_mapping": map[string]any{"gemini-2.5-flash": "gemini-2.5-flash"}},
					Status:      service.StatusActive, Schedulable: true, Concurrency: 20, Priority: int(id), AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}},
					Extra: map[string]any{"model_rate_limits": map[string]any{"gemini-2.5-flash": map[string]any{"rate_limit_reset_at": resetAt.Format(time.RFC3339Nano)}}}}
				accounts = append(accounts, a)
				persisted = append(persisted, *a)
			}
			cache := &poolRecoveryLoadCache{active: map[int64]int{}}
			concurrency := service.NewConcurrencyService(cache)
			cfg := &config.Config{RunMode: config.RunModeStandard}
			cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{StickySessionMaxWaiting: 64, StickySessionWaitTimeout: 30 * time.Second, FallbackWaitTimeout: 30 * time.Second, FallbackMaxWaiting: 64, LoadBatchEnabled: scenario.loadBatch}
			snapshot := service.NewSchedulerSnapshotService(&poolRecoverySnapshotCache{fakeSchedulerCache: fakeSchedulerCache{accounts: accounts}}, nil, nil, nil, nil)
			repo := &gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{groupID: persisted}}
			gateway := service.NewGatewayService(repo, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, cfg, snapshot, concurrency, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			h, cleanup := newTestGatewayHandler(t, group, accounts)
			defer cleanup()
			h.gatewayService = gateway
			h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, 0)
			var forwarded atomic.Int64
			upstream := &geminiWaitUpstream{do: func(req *http.Request, id int64) (*http.Response, error) {
				if time.Now().Before(resetAt) {
					t.Error("forwarded before account cooldown expired")
				}
				forwarded.Add(1)
				time.Sleep(20 * time.Second)
				if req.Context().Err() != nil {
					t.Error("successful generation canceled by admission budget")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`))}, nil
			}}
			h.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, upstream, nil, cfg)
			apiKey := &service.APIKey{ID: 1, UserID: 2, GroupID: &groupID, Group: group, Status: service.StatusActive, User: &service.User{ID: 2, Concurrency: 64, Balance: 100}}
			router := gin.New()
			router.POST("/v1beta/models/*modelAction", func(c *gin.Context) {
				c.Set(string(middleware.ContextKeyAPIKey), apiKey)
				c.Set("force_platform", service.PlatformAntigravity)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 64})
				h.GeminiV1BetaModels(c)
			})
			synctest.Test(t, func(t *testing.T) {
				start = time.Now()
				resetAt = start.Add(2 * time.Second)
				for _, a := range accounts {
					a.Extra["model_rate_limits"] = map[string]any{"gemini-2.5-flash": map[string]any{"rate_limit_reset_at": resetAt.Format(time.RFC3339Nano)}}
				}
				var wg sync.WaitGroup
				results := make([]int, scenario.count)
				for i := range scenario.count {
					wg.Go(func() {
						req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}]}`))
						ctx := context.WithValue(req.Context(), ctxkey.Group, group)
						ctx = context.WithValue(ctx, ctxkey.ForcePlatform, service.PlatformAntigravity)
						rec := httptest.NewRecorder()
						router.ServeHTTP(rec, req.WithContext(ctx))
						results[i] = rec.Code
						if rec.Code != 200 {
							t.Errorf("request %d: %d %s", i, rec.Code, rec.Body.String())
						}
					})
					if scenario.interval > 0 {
						time.Sleep(scenario.interval)
					}
				}
				wg.Wait()
				require.Equal(t, int64(scenario.count), forwarded.Load())
				for _, status := range results {
					require.Equal(t, http.StatusOK, status)
				}
				cache.mu.Lock()
				defer cache.mu.Unlock()
				require.LessOrEqual(t, cache.peak, 20)
				for _, active := range cache.active {
					require.Zero(t, active)
				}
				t.Logf("requests=%d successes=%d simulated_elapsed=%s peak_per_account=%d", scenario.count, forwarded.Load(), time.Since(start), cache.peak)
			})
		})
	}
}

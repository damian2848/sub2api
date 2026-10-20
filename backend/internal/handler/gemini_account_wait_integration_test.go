//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

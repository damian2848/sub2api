//go:build unit

package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// countingWaitCache tracks the live account wait count so a test can look at
// it while the acquire callback runs.
type countingWaitCache struct {
	fakeConcurrencyCache
	waiting    map[int64]int
	increments int
	decrements int
	failWith   error
}

func (c *countingWaitCache) IncrementAccountWaitCount(_ context.Context, accountID int64, maxWait int) (bool, error) {
	if c.failWith != nil {
		return false, c.failWith
	}
	if c.waiting[accountID] >= maxWait {
		return false, nil
	}
	c.waiting[accountID]++
	c.increments++
	return true, nil
}

func (c *countingWaitCache) DecrementAccountWaitCount(_ context.Context, accountID int64) error {
	c.waiting[accountID]--
	c.decrements++
	return nil
}

func TestWithAccountWaitCountCountsWhileWaiting(t *testing.T) {
	cache := &countingWaitCache{waiting: map[int64]int{}}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, 0)

	released := false
	release, err := helper.withAccountWaitCount(context.Background(), 7, 2, func() (func(), error) {
		require.Equal(t, 1, cache.waiting[7], "the request must be visible as waiting while it polls for a slot")
		return func() { released = true }, nil
	})

	require.NoError(t, err)
	require.NotNil(t, release)
	release()
	require.True(t, released)
	require.Zero(t, cache.waiting[7], "the wait count is returned once the slot is taken")
	require.Equal(t, 1, cache.increments)
	require.Equal(t, 1, cache.decrements)
}

func TestWithAccountWaitCountReturnsCountOnAcquireFailure(t *testing.T) {
	cache := &countingWaitCache{waiting: map[int64]int{}}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, 0)
	timeout := &ConcurrencyError{SlotType: "account", IsTimeout: true}

	release, err := helper.withAccountWaitCount(context.Background(), 7, 2, func() (func(), error) {
		return nil, timeout
	})

	require.Nil(t, release)
	require.ErrorIs(t, err, timeout)
	require.Zero(t, cache.waiting[7])
}

func TestWithAccountWaitCountRejectsFullQueueWithoutWaiting(t *testing.T) {
	cache := &countingWaitCache{waiting: map[int64]int{7: 2}}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, 0)

	called := false
	release, err := helper.withAccountWaitCount(context.Background(), 7, 2, func() (func(), error) {
		called = true
		return func() {}, nil
	})

	require.Nil(t, release)
	var queueFull *WaitQueueFullError
	require.True(t, errors.As(err, &queueFull))
	require.Equal(t, "account", queueFull.SlotType)
	require.False(t, called, "a full queue must not start polling for a slot")
	require.Equal(t, 2, cache.waiting[7])
	status, _, code, _ := concurrencyErrorResponse(err, "account")
	require.Equal(t, http.StatusTooManyRequests, status)
	require.Equal(t, gatewayQueueFullCode, code)
}

// The concurrency service itself fails open on counter errors; the helper keeps
// waiting in that case instead of rejecting the request.
func TestWithAccountWaitCountFailsOpenOnCounterError(t *testing.T) {
	cache := &countingWaitCache{waiting: map[int64]int{}, failWith: errors.New("redis down")}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, 0)

	called := false
	release, err := helper.withAccountWaitCount(context.Background(), 7, 1, func() (func(), error) {
		called = true
		return func() {}, nil
	})

	require.NoError(t, err)
	require.NotNil(t, release)
	require.True(t, called)
}

func runWaitPlanEndpoint(t *testing.T, h *GatewayHandler, group *service.Group, path, body string, serve func(*GatewayHandler, *gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	groupID := group.ID
	apiKey := &service.APIKey{
		ID: 3201, UserID: 4201, GroupID: &groupID, Status: service.StatusActive,
		User:  &service.User{ID: 4201, Concurrency: 10, Balance: 100},
		Group: group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	serve(h, c)
	return rec
}

func waitPlanAPIKeyAccount(groupID int64) *service.Account {
	accountID := int64(1201)
	return &service.Account{
		ID:            accountID,
		Name:          "apikey-busy",
		Platform:      service.PlatformAnthropic,
		Type:          service.AccountTypeAPIKey,
		Credentials:   map[string]any{"api_key": "sk-test"},
		Concurrency:   1,
		Priority:      1,
		Status:        service.StatusActive,
		Schedulable:   true,
		AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}
}

func TestGatewayHandlerResponsesAndChatCompletionsRespectAccountWaitQueue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name  string
		path  string
		body  string
		serve func(*GatewayHandler, *gin.Context)
	}{
		{
			name:  "responses",
			path:  "/v1/responses",
			body:  `{"model":"claude-sonnet-4-5","input":"hello"}`,
			serve: (*GatewayHandler).Responses,
		},
		{
			name:  "chat_completions",
			path:  "/v1/chat/completions",
			body:  `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`,
			serve: (*GatewayHandler).ChatCompletions,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name+"/queue_full", func(t *testing.T) {
			group := &service.Group{ID: 2201 + int64(i)*10, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			account := waitPlanAPIKeyAccount(group.ID)
			helperCache := &slotConcurrencyCache{acquireAccount: true, canWait: false}
			h, cleanup := newWaitPlanTestHandler(t, group, account,
				&slotConcurrencyCache{acquireAccount: false},
				helperCache,
				newRecordingSessionLimitCache(),
			)
			defer cleanup()

			rec := runWaitPlanEndpoint(t, h, group, tc.path, tc.body, tc.serve)

			require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), gatewayQueueFullCode)
			inc, dec := helperCache.waitCounts()
			require.Zero(t, inc)
			require.Zero(t, dec)
		})
		t.Run(tc.name+"/wait_timeout_returns_count", func(t *testing.T) {
			group := &service.Group{ID: 2202 + int64(i)*10, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			account := waitPlanAPIKeyAccount(group.ID)
			helperCache := &slotConcurrencyCache{acquireAccount: false, canWait: true}
			h, cleanup := newWaitPlanTestHandler(t, group, account,
				&slotConcurrencyCache{acquireAccount: false},
				helperCache,
				newRecordingSessionLimitCache(),
			)
			defer cleanup()

			rec := runWaitPlanEndpoint(t, h, group, tc.path, tc.body, tc.serve)

			require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
			inc, dec := helperCache.waitCounts()
			require.Equal(t, 1, inc, "the waiting request is counted against the account queue")
			require.Equal(t, 1, dec, "and released when the wait ends")
		})
	}
}

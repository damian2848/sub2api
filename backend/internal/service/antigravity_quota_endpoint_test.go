package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

type antigravityQuotaEndpointRequests struct {
	summary atomic.Int32
	models  atomic.Int32
}

func newAntigravityQuotaEndpointServer(t *testing.T, daily bool) (*httptest.Server, *antigravityQuotaEndpointRequests) {
	t.Helper()
	requests := &antigravityQuotaEndpointRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1internal:retrieveUserQuotaSummary":
			requests.summary.Add(1)
			// Upstream fractions are remaining quota: daily becomes 41.11%
			// session usage and 98.59% weekly usage; production remains unused.
			session, weekly := 1.0, 1.0
			if daily {
				session, weekly = 0.5889, 0.0141
			}
			_, _ = fmt.Fprintf(w, `{"groups":[{"displayName":"Gemini","buckets":[{"bucketId":"session","window":"5h","remainingFraction":%f},{"bucketId":"week","window":"weekly","remainingFraction":%f}]}]}`, session, weekly)
		case "/v1internal:fetchAvailableModels":
			requests.models.Add(1)
			remaining := 1.0
			if daily {
				remaining = 0.5
			}
			_, _ = fmt.Fprintf(w, `{"models":{"gemini-4-argon":{"quotaInfo":{"remainingFraction":%f,"resetTime":%q}}}}`, remaining, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case "/v1internal:loadCodeAssist":
			_, _ = w.Write([]byte(`{"currentTier":{"id":"g1-pro-tier"}}`))
		default:
			t.Errorf("unexpected quota endpoint request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func setAntigravityQuotaTestEndpoints(t *testing.T, urls ...string) {
	t.Helper()
	oldURLs, oldAvailability := antigravity.BaseURLs, antigravity.DefaultURLAvailability
	antigravity.BaseURLs = urls
	antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)
	t.Cleanup(func() {
		antigravity.BaseURLs, antigravity.DefaultURLAvailability = oldURLs, oldAvailability
	})
	t.Setenv(antigravityForwardBaseURLEnv, "")
}

func antigravityQuotaEndpointAccount(plan string) *Account {
	return &Account{
		ID: 4242, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "test-token", "project_id": "test-project", "plan_type": plan},
	}
}

func requireAntigravityEndpointQuota(t *testing.T, usage *UsageInfo, daily bool) {
	t.Helper()
	require.NotNil(t, usage)
	require.Empty(t, usage.Error)
	require.Len(t, usage.AntigravityQuotaGroups, 1)
	require.Len(t, usage.AntigravityQuotaGroups[0].Buckets, 2)
	session, weekly, model := 0.0, 0.0, 0
	if daily {
		session, weekly, model = 41.11, 98.59, 50
	}
	require.InDelta(t, session, *usage.AntigravityQuotaGroups[0].Buckets[0].Utilization, 0.000001)
	require.InDelta(t, weekly, *usage.AntigravityQuotaGroups[0].Buckets[1].Utilization, 0.000001)
	require.Equal(t, model, usage.AntigravityQuota["gemini-4-argon"].Utilization)
}

func TestAntigravityQuotaUsesForwardingEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, plan, override string
		daily                bool
	}{
		{name: "paid plan selects daily", plan: "pro", daily: true},
		{name: "free plan selects production", plan: "free"},
		{name: "daily override selects daily for free", plan: "free", override: "daily", daily: true},
		{name: "production override selects production for paid", plan: "pro", override: "prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			production, prodRequests := newAntigravityQuotaEndpointServer(t, false)
			daily, dailyRequests := newAntigravityQuotaEndpointServer(t, true)
			setAntigravityQuotaTestEndpoints(t, production.URL, daily.URL)
			t.Setenv(antigravityForwardBaseURLEnv, tc.override)
			result, err := NewAntigravityQuotaFetcher(nil, nil).FetchQuota(context.Background(), antigravityQuotaEndpointAccount(tc.plan), "")
			require.NoError(t, err)
			requireAntigravityEndpointQuota(t, result.UsageInfo, tc.daily)
			selected, other := prodRequests, dailyRequests
			if tc.daily {
				selected, other = dailyRequests, prodRequests
			}
			require.Equal(t, int32(1), selected.summary.Load())
			require.Equal(t, int32(1), selected.models.Load())
			require.Zero(t, other.summary.Load(), "quota summary must not be read from another pool")
			require.Zero(t, other.models.Load(), "model quota must use the same pool as the summary")
		})
	}
}

func TestAntigravityQuotaEndpointFailureDoesNotUseAnotherPool(t *testing.T) {
	for _, tc := range []struct {
		name     string
		redirect bool
	}{
		{name: "server failure"},
		{name: "redirect to production", redirect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			production, prodRequests := newAntigravityQuotaEndpointServer(t, false)
			var dailyRequests atomic.Int32
			daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dailyRequests.Add(1)
				if tc.redirect {
					http.Redirect(w, r, production.URL+r.URL.Path, http.StatusTemporaryRedirect)
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"daily unavailable"}`))
			}))
			t.Cleanup(daily.Close)
			setAntigravityQuotaTestEndpoints(t, production.URL, daily.URL)
			result, err := NewAntigravityQuotaFetcher(nil, nil).FetchQuota(context.Background(), antigravityQuotaEndpointAccount("pro"), "")
			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, int32(2), dailyRequests.Load())
			require.Zero(t, prodRequests.summary.Load())
			require.Zero(t, prodRequests.models.Load())
		})
	}
}

func TestAntigravityUsageCacheChangesWithForwardingEndpoint(t *testing.T) {
	production, prodRequests := newAntigravityQuotaEndpointServer(t, false)
	daily, dailyRequests := newAntigravityQuotaEndpointServer(t, true)
	setAntigravityQuotaTestEndpoints(t, production.URL, daily.URL)
	account := antigravityQuotaEndpointAccount("pro")
	svc := &AccountUsageService{antigravityQuotaFetcher: NewAntigravityQuotaFetcher(nil, nil), cache: NewUsageCache()}
	usage, err := svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	requireAntigravityEndpointQuota(t, usage, true)

	account.Credentials["plan_type"] = "free"
	ops := &AccountOpsService{usageCache: svc.cache}
	windows := ops.usageWindows(context.Background(), account, time.Now())
	require.Len(t, windows, 1)
	require.Equal(t, "unsupported", windows[0].Status, "notification reads must also reject another pool's snapshot")
	usage, err = svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	requireAntigravityEndpointQuota(t, usage, false)

	t.Setenv(antigravityForwardBaseURLEnv, "daily")
	usage, err = svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	requireAntigravityEndpointQuota(t, usage, true)
	usage, err = svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	requireAntigravityEndpointQuota(t, usage, true)
	require.Equal(t, int32(1), prodRequests.summary.Load())
	require.Equal(t, int32(1), prodRequests.models.Load())
	require.Equal(t, int32(2), dailyRequests.summary.Load())
	require.Equal(t, int32(2), dailyRequests.models.Load())
}

func TestAntigravityUsageDoesNotJoinAnotherEndpointFlight(t *testing.T) {
	production, prodRequests := newAntigravityQuotaEndpointServer(t, false)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1internal:retrieveUserQuotaSummary" {
			_, _ = w.Write([]byte(`{"groups":[{"displayName":"Daily","buckets":[{"bucketId":"daily","remainingFraction":0.5}]}]}`))
		} else {
			_, _ = w.Write([]byte(`{"models":{}}`))
		}
	}))
	t.Cleanup(daily.Close)
	setAntigravityQuotaTestEndpoints(t, production.URL, daily.URL)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	svc := &AccountUsageService{antigravityQuotaFetcher: NewAntigravityQuotaFetcher(nil, nil), cache: NewUsageCache()}
	done := make(chan error, 1)
	go func() {
		_, err := svc.GetUsageForAccount(context.Background(), antigravityQuotaEndpointAccount("pro"))
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("daily query did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	usage, err := svc.GetUsageForAccount(ctx, antigravityQuotaEndpointAccount("free"))
	require.NoError(t, err, "production query must not wait for the same account's daily flight")
	requireAntigravityEndpointQuota(t, usage, false)
	require.Equal(t, int32(1), prodRequests.summary.Load())
	unblock()
	require.NoError(t, <-done)
}

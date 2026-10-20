package service

import (
	"context"
	"encoding/json"
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

type antigravityUsageAccountRepo struct {
	AccountRepository
	account    *Account
	clearCalls int
}

func (r *antigravityUsageAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *antigravityUsageAccountRepo) GetByIDs(context.Context, []int64) ([]*Account, error) {
	return []*Account{r.account}, nil
}

func (r *antigravityUsageAccountRepo) ClearError(context.Context, int64) error {
	r.clearCalls++
	return nil
}

func newAntigravityUsageTestService(t *testing.T, modelsHandler http.HandlerFunc) (*AccountUsageService, *Account) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1internal:fetchAvailableModels" {
			modelsHandler(w, r)
			return
		}
		if r.URL.Path == "/v1internal:retrieveUserQuotaSummary" {
			_, _ = w.Write([]byte(`{"groups":[{"displayName":"Gemini","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5}]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	oldBaseURLs := antigravity.BaseURLs
	oldAvailability := antigravity.DefaultURLAvailability
	antigravity.BaseURLs = []string{server.URL}
	antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)
	t.Cleanup(func() {
		server.Close()
		antigravity.BaseURLs = oldBaseURLs
		antigravity.DefaultURLAvailability = oldAvailability
	})
	account := &Account{
		ID: 42, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "test-token", "project_id": "test-project"},
	}
	return &AccountUsageService{
		accountRepo:             &antigravityUsageAccountRepo{account: account},
		antigravityQuotaFetcher: NewAntigravityQuotaFetcher(nil, nil),
		cache:                   NewUsageCache(),
	}, account
}

func writeAntigravityUsageTestModels(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"models": map[string]any{
			"gemini-2.5-pro": map[string]any{
				"quotaInfo": map[string]any{
					"remainingFraction": 0.5,
					"resetTime":         time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				},
			},
		},
	})
}

func TestAntigravityUsageForceBypassesCachedSuccessAndErrors(t *testing.T) {
	for _, entrypoint := range []string{"single", "loaded_account", "batch"} {
		for _, cachedError := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/error=%t", entrypoint, cachedError), func(t *testing.T) {
				var requests atomic.Int32
				svc, account := newAntigravityUsageTestService(t, func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					writeAntigravityUsageTestModels(w)
				})
				updatedAt := time.Now().Add(-30 * time.Second)
				cached := &UsageInfo{
					UpdatedAt: &updatedAt,
					AntigravityQuota: map[string]*AntigravityModelQuota{
						"gemini-2.5-pro": {Utilization: 10},
					},
				}
				if cachedError {
					cached.Error = "previous query failed"
					cached.ErrorCode = errorCodeNetworkError
					account.Status = StatusError
					account.ErrorMessage = "Token refresh failed: unauthenticated"
				}
				svc.cache.antigravityCache.Store(account.ID, &antigravityUsageCache{usageInfo: cached, timestamp: updatedAt, baseURL: resolveAntigravityForwardBaseURL(account)})
				query := func(force bool) (*UsageInfo, error) {
					switch entrypoint {
					case "single":
						return svc.GetUsage(context.Background(), account.ID, force)
					case "loaded_account":
						return svc.GetUsageForAccount(context.Background(), account, force)
					default:
						usage, failures, err := svc.GetUsageBatch(context.Background(), []int64{account.ID}, force)
						require.Empty(t, failures)
						return usage[account.ID], err
					}
				}
				oldUsage, err := query(false)
				require.NoError(t, err)
				require.Equal(t, int32(0), requests.Load())
				require.True(t, oldUsage.UpdatedAt.Equal(updatedAt))
				require.Equal(t, 10, oldUsage.AntigravityQuota["gemini-2.5-pro"].Utilization)
				require.Zero(t, svc.accountRepo.(*antigravityUsageAccountRepo).clearCalls, "cached failures must not clear account errors")

				freshUsage, err := query(true)
				require.NoError(t, err)
				require.Equal(t, int32(1), requests.Load())
				require.Empty(t, freshUsage.Error)
				require.Empty(t, freshUsage.ErrorCode)
				require.Equal(t, 50, freshUsage.AntigravityQuota["gemini-2.5-pro"].Utilization)
				require.True(t, freshUsage.UpdatedAt.After(updatedAt))
				if cachedError {
					require.Equal(t, 1, svc.accountRepo.(*antigravityUsageAccountRepo).clearCalls)
				}

				nextUsage, err := query(false)
				require.NoError(t, err)
				require.Equal(t, int32(1), requests.Load())
				require.True(t, nextUsage.UpdatedAt.Equal(*freshUsage.UpdatedAt))
				require.Equal(t, 50, nextUsage.AntigravityQuota["gemini-2.5-pro"].Utilization)
			})
		}
	}
}

func TestAntigravityUsageCacheReturnsIndependentSnapshots(t *testing.T) {
	svc, account := newAntigravityUsageTestService(t, func(http.ResponseWriter, *http.Request) {
		t.Error("a cached query must not contact the upstream")
	})
	updatedAt := time.Now().Add(-30 * time.Second)
	resetAt := time.Now().Add(time.Hour)
	expiredAt := time.Now().Add(-time.Hour)
	supportsImages := true
	utilization := 20.0
	cached := &UsageInfo{
		UpdatedAt: &updatedAt,
		FiveHour:  &UsageProgress{Utilization: 20, ResetsAt: &resetAt, RemainingSeconds: 9999},
		SevenDay:  &UsageProgress{Utilization: 30, ResetsAt: &expiredAt, RemainingSeconds: 9999},
		AntigravityQuota: map[string]*AntigravityModelQuota{
			"gemini-2.5-pro": {Utilization: 20},
		},
		AntigravityQuotaGroups: []AntigravityQuotaGroup{{
			GroupID: "gemini", Buckets: []AntigravityQuotaBucket{{BucketID: "5h", Utilization: &utilization}},
		}},
		AntigravityQuotaDetails: map[string]*AntigravityModelDetail{
			"gemini-2.5-pro": {SupportsImages: &supportsImages, SupportedMimeTypes: map[string]bool{"image/png": true}},
		},
		AICredits:            []AICredit{{Amount: 25}},
		ModelForwardingRules: map[string]string{"old": "new"},
	}
	svc.cache.antigravityCache.Store(account.ID, &antigravityUsageCache{usageInfo: cached, timestamp: updatedAt, baseURL: resolveAntigravityForwardBaseURL(account)})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			usage, err := svc.GetUsageForAccount(context.Background(), account)
			if err != nil {
				t.Error(err)
				return
			}
			if usage.FiveHour.RemainingSeconds > 3600 || usage.FiveHour.RemainingSeconds < 3590 || usage.SevenDay.RemainingSeconds != 0 {
				t.Errorf("incorrect cached countdowns: %#v, %#v", usage.FiveHour, usage.SevenDay)
			}
			*usage.UpdatedAt = time.Time{}
			usage.FiveHour.Utilization = 100
			*usage.FiveHour.ResetsAt = time.Time{}
			usage.AntigravityQuota["gemini-2.5-pro"].Utilization = 100
			usage.AntigravityQuotaGroups[0].GroupID = "changed"
			*usage.AntigravityQuotaGroups[0].Buckets[0].Utilization = 100
			*usage.AntigravityQuotaDetails["gemini-2.5-pro"].SupportsImages = false
			usage.AntigravityQuotaDetails["gemini-2.5-pro"].SupportedMimeTypes["image/png"] = false
			usage.AICredits[0].Amount = 0
			usage.ModelForwardingRules["old"] = "changed"
		})
	}
	wg.Wait()
	require.Equal(t, updatedAt, *cached.UpdatedAt)
	require.Equal(t, 9999, cached.FiveHour.RemainingSeconds)
	require.Equal(t, 9999, cached.SevenDay.RemainingSeconds)
	require.Equal(t, float64(20), cached.FiveHour.Utilization)
	require.Equal(t, resetAt, *cached.FiveHour.ResetsAt)
	require.Equal(t, 20, cached.AntigravityQuota["gemini-2.5-pro"].Utilization)
	require.Equal(t, "gemini", cached.AntigravityQuotaGroups[0].GroupID)
	require.Equal(t, float64(20), *cached.AntigravityQuotaGroups[0].Buckets[0].Utilization)
	require.True(t, *cached.AntigravityQuotaDetails["gemini-2.5-pro"].SupportsImages)
	require.True(t, cached.AntigravityQuotaDetails["gemini-2.5-pro"].SupportedMimeTypes["image/png"])
	require.Equal(t, float64(25), cached.AICredits[0].Amount)
	require.Equal(t, "new", cached.ModelForwardingRules["old"])
}

func TestAntigravityUsageCancelledCallerDoesNotCancelSharedRefresh(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var requests atomic.Int32
	svc, account := newAntigravityUsageTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		started <- struct{}{}
		<-release
		writeAntigravityUsageTestModels(w)
	})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := svc.GetUsageForAccount(ctx, account, true)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream query did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancelled caller kept waiting for the shared query")
	}

	ctx, cancelSecond := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelSecond()
	_, err := svc.GetUsageForAccount(ctx, account, true)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, int32(1), requests.Load(), "concurrent refreshes must share the upstream request")
	unblock()
	require.Eventually(t, func() bool {
		_, ok := svc.cache.antigravityCache.Load(account.ID)
		return ok
	}, 3*time.Second, 10*time.Millisecond)
	usage, err := svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, 50, usage.AntigravityQuota["gemini-2.5-pro"].Utilization)
	require.Equal(t, int32(1), requests.Load())
}

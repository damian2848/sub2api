//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

// Synthetic values using the official quota_summary.proto REST field names.
const groupedAntigravityQuotaFixture = `{"groups":[
 {"displayName":"Gemini models","description":"Shared Gemini pool","buckets":[
  {"bucketId":"gemini-session","displayName":"5 hours","window":"5h","remainingFraction":0.8,"resetTime":"2026-10-20T12:00:00Z"},
  {"bucketId":"gemini-week","displayName":"Weekly","window":"weekly","remainingFraction":0.4,"resetTime":"2026-10-26T12:00:00Z"}]},
 {"displayName":"GPT & Claude models","buckets":[
  {"bucketId":"other-session","window":"5h","remainingFraction":0,"resetTime":"2026-10-20T14:00:00Z"},
  {"bucketId":"other-week","window":"weekly","remainingFraction":1,"resetTime":"2026-10-27T12:00:00Z"}]},
 {"displayName":"gemini-4-argon","buckets":[
  {"bucketId":"argon","window":"preview","remainingFraction":0.125,"resetTime":"2026-10-21T12:00:00Z"}]}]}`

func TestAntigravityQuotaGroupsPreserveIndependentWindows(t *testing.T) {
	var summary antigravity.RetrieveUserQuotaSummaryResponse
	require.NoError(t, json.Unmarshal([]byte(groupedAntigravityQuotaFixture), &summary))
	groups := buildAntigravityQuotaGroups(&summary)
	require.Len(t, groups, 3)
	require.Equal(t, "Shared Gemini pool", groups[0].Description)
	require.Len(t, groups[0].Buckets, 2)
	require.InDelta(t, 20, *groups[0].Buckets[0].Utilization, 0.001)
	require.InDelta(t, 60, *groups[0].Buckets[1].Utilization, 0.001)
	require.Equal(t, "2026-10-20T12:00:00Z", groups[0].Buckets[0].ResetTime)
	require.Equal(t, "2026-10-26T12:00:00Z", groups[0].Buckets[1].ResetTime)
	require.Equal(t, 100.0, *groups[1].Buckets[0].Utilization)
	require.Zero(t, *groups[1].Buckets[1].Utilization)
	require.Equal(t, "gemini-4-argon", groups[2].DisplayName)
	require.Equal(t, "preview", groups[2].Buckets[0].Window)
	require.Equal(t, 87.5, *groups[2].Buckets[0].Utilization)
}

func TestAntigravityQuotaGroupsMissingRemainingIsUnknown(t *testing.T) {
	var summary antigravity.RetrieveUserQuotaSummaryResponse
	require.NoError(t, json.Unmarshal([]byte(`{"buckets":[
 {"bucketId":"unknown"},
 {"bucketId":"disabled","disabled":true},
 {"bucketId":"amount","remainingAmount":"0"},
 {"bucketId":"invalid","remainingFraction":-0.1}
 ]}`), &summary))
	groups := buildAntigravityQuotaGroups(&summary)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Buckets, 4)
	for _, bucket := range groups[0].Buckets {
		require.Nil(t, bucket.Utilization, "missing fractions and absolute amounts must not become 100%% used")
	}
	require.True(t, groups[0].Buckets[1].Disabled)
	require.NotNil(t, groups[0].Buckets[2].RemainingAmount)
	require.Zero(t, *groups[0].Buckets[2].RemainingAmount)
	// A legacy top-level duplicate must not appear beside authoritative groups.
	summary.Groups = []antigravity.QuotaSummaryGroup{{DisplayName: "group", Buckets: summary.Buckets[:1]}}
	groups = buildAntigravityQuotaGroups(&summary)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Buckets, 1)
}

func TestFetchAntigravityQuotaPrefersSummaryAndReportsFallback(t *testing.T) {
	for _, tc := range []struct {
		name          string
		summaryStatus int
		modelsStatus  int
	}{
		{"official summary", http.StatusOK, http.StatusOK},
		{"optional catalog unavailable", http.StatusOK, http.StatusForbidden},
		{"legacy fallback", http.StatusNotFound, http.StatusOK},
		{"summary temporarily unavailable", http.StatusTooManyRequests, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1internal:retrieveUserQuotaSummary":
					w.WriteHeader(tc.summaryStatus)
					if tc.summaryStatus == http.StatusOK {
						_, _ = w.Write([]byte(groupedAntigravityQuotaFixture))
					} else {
						_, _ = w.Write([]byte(`{"error":"unavailable"}`))
					}
				case "/v1internal:fetchAvailableModels":
					w.WriteHeader(tc.modelsStatus)
					_, _ = w.Write([]byte(`{"models":{"gemini-4-argon":{"quotaInfo":{"remainingFraction":1}}}}`))
				case "/v1internal:loadCodeAssist":
					_, _ = w.Write([]byte(`{"currentTier":{"id":"g1-pro-tier"}}`))
				default:
					t.Errorf("unexpected request path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			oldURLs, oldAvailability := antigravity.BaseURLs, antigravity.DefaultURLAvailability
			antigravity.BaseURLs = []string{server.URL}
			antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)
			t.Cleanup(func() {
				server.Close()
				antigravity.BaseURLs, antigravity.DefaultURLAvailability = oldURLs, oldAvailability
			})
			started := time.Now()
			result, err := NewAntigravityQuotaFetcher(nil, nil).FetchQuota(context.Background(), &Account{
				Platform: PlatformAntigravity, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "test-token", "project_id": "test-project"},
			}, "")
			require.NoError(t, err)
			usage := result.UsageInfo
			require.Equal(t, "active", usage.Source)
			require.Equal(t, "PRO", usage.SubscriptionTier)
			require.NotNil(t, usage.UpdatedAt)
			require.False(t, usage.UpdatedAt.Before(started))
			require.Nil(t, usage.FiveHour)
			require.False(t, usage.IsForbidden)
			if tc.summaryStatus == http.StatusOK {
				require.Equal(t, "quota_summary", usage.AntigravityQuotaSource)
				require.Empty(t, usage.AntigravityQuotaError)
				require.Len(t, usage.AntigravityQuotaGroups, 3)
				require.Contains(t, result.Raw, "quotaSummary")
			} else {
				require.Equal(t, "model_quota", usage.AntigravityQuotaSource)
				require.Equal(t, "quota_summary_unavailable", usage.AntigravityQuotaError)
				require.Empty(t, usage.AntigravityQuotaGroups)
				require.Contains(t, usage.AntigravityQuota, "gemini-4-argon")
			}
		})
	}
}

func TestMonitorQuotaUsesAntigravitySummaryWithoutLegacyDuplicates(t *testing.T) {
	var summary antigravity.RetrieveUserQuotaSummaryResponse
	require.NoError(t, json.Unmarshal([]byte(groupedAntigravityQuotaFixture), &summary))
	tiers := usageQuotaTiers(&UsageInfo{
		AntigravityQuotaGroups: buildAntigravityQuotaGroups(&summary),
		AntigravityQuota:       map[string]*AntigravityModelQuota{"gemini-4-argon": {Utilization: 0}},
	})
	require.Len(t, tiers, 5)
	require.Equal(t, "5h", tiers[0].Window)
	require.Equal(t, "7d", tiers[1].Window)
	require.Equal(t, "Gemini models / Weekly", tiers[1].Label)
	require.Equal(t, "preview", tiers[4].Window)
	require.Equal(t, 87.5, tiers[4].UsedPercent)
}

func TestAntigravityIncompleteSummaryUsesShortCacheAndCannotReportHealthy(t *testing.T) {
	queriedAt := time.Now().Add(-2 * time.Minute)
	for _, usage := range []*UsageInfo{
		{
			UpdatedAt: &queriedAt, AntigravityQuotaSource: "model_quota",
			AntigravityQuotaError: "quota_summary_unavailable",
			AntigravityQuota:      map[string]*AntigravityModelQuota{"gemini-4-argon": {Utilization: 0}},
		},
		{
			UpdatedAt: &queriedAt, AntigravityQuotaSource: "quota_summary",
			AntigravityQuotaGroups: []AntigravityQuotaGroup{{Buckets: []AntigravityQuotaBucket{{BucketID: "unknown"}}}},
		},
	} {
		fetcher := &ChannelMonitorQuotaFetcher{usage: &stubMonitorUsageSource{usage: usage}}
		snapshot := fetcher.fetchUsage(context.Background(), &Account{Platform: PlatformAntigravity}, time.Now())
		require.False(t, snapshot.Success, "unknown windows must not imply a healthy quota")
		require.NotEmpty(t, snapshot.Error)
		require.Equal(t, queriedAt, snapshot.FetchedAt)
		if usage.AntigravityQuotaError != "" {
			require.Equal(t, antigravityErrorTTL, antigravityCacheTTL(usage))
		}
	}
}

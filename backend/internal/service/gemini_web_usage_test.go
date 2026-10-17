package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

type geminiWebUsageHTTPStub struct {
	HTTPUpstream
	do func(*http.Request) (*http.Response, error)
}

func (h *geminiWebUsageHTTPStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return h.do(req)
}

func TestGeminiWebUsageAllowsSlowQueriesAndForcesFreshQuota(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries, bootstraps := 0, 0
		upstream := &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
			body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
			if req.Method == http.MethodGet {
				bootstraps++
			} else {
				queries++
				select {
				case <-time.After(5 * time.Second):
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				fraction := "0.25"
				if queries == 2 {
					fraction = "0.5"
				}
				payload, _ := json.Marshal([]any{2, []any{
					[]any{"", fraction, 1, []any{"1730000000", "0"}},
					[]any{"", "0.75", 2, []any{"1731000000", "0"}},
				}})
				outer, _ := json.Marshal([]any{[]any{"wrb.fr", "jSf9Qc", string(payload)}})
				body = ")]}'\n" + string(outer)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}}
		svc := &AccountUsageService{httpUpstream: upstream, geminiWeb: geminiweb.NewClient()}
		account := &Account{ID: 42, Platform: PlatformGemini, Type: AccountTypeGeminiWeb,
			Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"}}
		usage, err := svc.getGeminiWebUsage(context.Background(), account, false)
		if err != nil || usage.Error != "" || usage.FiveHour == nil || usage.SevenDay == nil {
			t.Fatalf("slow manual query failed: usage=%#v error=%v", usage, err)
		}
		if usage.FiveHour.Utilization != 25 || usage.SevenDay.Utilization != 75 {
			t.Fatalf("unexpected initial usage: five_hour=%#v seven_day=%#v", usage.FiveHour, usage.SevenDay)
		}
		usage, err = svc.getGeminiWebUsage(context.Background(), account, true)
		if err != nil || usage.Error != "" || usage.FiveHour == nil || usage.FiveHour.Utilization != 50 {
			t.Fatalf("forced query did not return fresh quota: usage=%#v error=%v", usage, err)
		}
		if queries != 2 || bootstraps != 1 {
			t.Fatalf("usage requests: queries=%d bootstraps=%d, want 2 and 1", queries, bootstraps)
		}
	})
}

func TestGeminiWebUsageTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstream := &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}}
		svc := &AccountUsageService{httpUpstream: upstream, geminiWeb: geminiweb.NewClient()}
		account := &Account{Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"}}
		start := time.Now()
		usage, err := svc.getGeminiWebUsage(context.Background(), account, true)
		if err != nil || usage.ErrorCode != "network_error" || usage.Error == "" {
			t.Fatalf("timeout was not reported: usage=%#v error=%v", usage, err)
		}
		if elapsed := time.Since(start); elapsed != 20*time.Second {
			t.Fatalf("manual quota timeout = %v, want 20s", elapsed)
		}
	})
}

type geminiWebFirstUsageRepo struct {
	UsageLogRepository
	first  *time.Time
	starts []time.Time
}

func (r *geminiWebFirstUsageRepo) GetAccountFirstUsageSince(_ context.Context, _ int64, start time.Time) (*time.Time, error) {
	r.starts = append(r.starts, start)
	return r.first, nil
}

func geminiWebQuotaStub(buckets ...[]any) *geminiWebUsageHTTPStub {
	return &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
		body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
		if req.Method != http.MethodGet {
			list := make([]any, len(buckets))
			for i, b := range buckets {
				list[i] = b
			}
			payload, _ := json.Marshal([]any{2, list})
			outer, _ := json.Marshal([]any{[]any{"wrb.fr", "jSf9Qc", string(payload)}})
			body = ")]}'\n" + string(outer)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}}
}

func TestGeminiWebUsageEstimatesMissingResetFromFirstLocalRequest(t *testing.T) {
	now := time.Now()
	first := now.Add(-2 * time.Hour)
	weeklyReset := now.Add(3 * 24 * time.Hour).Truncate(time.Second)
	repo := &geminiWebFirstUsageRepo{first: &first}
	svc := &AccountUsageService{
		httpUpstream: geminiWebQuotaStub(
			[]any{"", "0.25", 1},
			[]any{"", "0.5", 2, []any{strconv.FormatInt(weeklyReset.Unix(), 10), "0"}},
		),
		geminiWeb:    geminiweb.NewClient(),
		usageLogRepo: repo,
	}
	account := &Account{ID: 7, Platform: PlatformGemini, Type: AccountTypeGeminiWeb,
		Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"}}
	usage, err := svc.getGeminiWebUsage(context.Background(), account, false)
	if err != nil || usage.Error != "" || usage.FiveHour == nil || usage.SevenDay == nil {
		t.Fatalf("usage=%#v error=%v", usage, err)
	}
	if usage.SubscriptionTier != "PRO" || usage.SubscriptionTierRaw != "2" {
		t.Fatalf("subscription tier = %q (raw %q), want PRO (2)", usage.SubscriptionTier, usage.SubscriptionTierRaw)
	}
	five := usage.FiveHour
	if five.ResetsAt == nil || !five.ResetsAt.Equal(first.Add(5*time.Hour)) || !five.ResetsAtEstimated {
		t.Fatalf("five-hour reset was not estimated from the first request: %#v", five)
	}
	if five.RemainingSeconds < 3*3600-5 || five.RemainingSeconds > 3*3600 {
		t.Fatalf("five-hour remaining seconds = %d, want about 3h", five.RemainingSeconds)
	}
	seven := usage.SevenDay
	if seven.ResetsAt == nil || !seven.ResetsAt.Equal(weeklyReset) || seven.ResetsAtEstimated {
		t.Fatalf("upstream weekly reset must be kept as-is: %#v", seven)
	}
	if len(repo.starts) != 1 || repo.starts[0].Sub(now.Add(-5*time.Hour)).Abs() > time.Second {
		t.Fatalf("first-usage lookups = %v, want one lookup over the last 5h", repo.starts)
	}
}

func TestGeminiWebUsageLeavesResetEmptyWithoutEvidence(t *testing.T) {
	now := time.Now()
	expired := now.Add(-7 * time.Hour)
	cases := []struct {
		name     string
		fraction string
		first    *time.Time
	}{
		{name: "unused window", fraction: "0", first: &expired},
		{name: "no local requests", fraction: "0.4"},
		{name: "estimate already passed", fraction: "0.4", first: &expired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &AccountUsageService{
				httpUpstream: geminiWebQuotaStub([]any{"", tc.fraction, 1}),
				geminiWeb:    geminiweb.NewClient(),
				usageLogRepo: &geminiWebFirstUsageRepo{first: tc.first},
			}
			account := &Account{ID: 7, Platform: PlatformGemini, Type: AccountTypeGeminiWeb,
				Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"}}
			usage, err := svc.getGeminiWebUsage(context.Background(), account, false)
			if err != nil || usage.FiveHour == nil {
				t.Fatalf("usage=%#v error=%v", usage, err)
			}
			if usage.FiveHour.ResetsAt != nil || usage.FiveHour.ResetsAtEstimated {
				t.Fatalf("reset must stay unknown: %#v", usage.FiveHour)
			}
		})
	}
}

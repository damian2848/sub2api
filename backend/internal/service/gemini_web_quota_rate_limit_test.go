package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/stretchr/testify/require"
)

type geminiWebQuotaRateLimitRepo struct {
	AccountRepository
	setCalls   []time.Time
	clearCalls int
}

func (r *geminiWebQuotaRateLimitRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	r.setCalls = append(r.setCalls, resetAt)
	return nil
}

func (r *geminiWebQuotaRateLimitRepo) ClearRateLimit(_ context.Context, _ int64) error {
	r.clearCalls++
	return nil
}

func geminiWebTestWindow(id string, usedPercent float64, resetAt *time.Time) geminiweb.QuotaWindow {
	w := geminiweb.QuotaWindow{ID: id, UsedPercent: usedPercent, RemainingPercent: 100 - usedPercent, HasPercent: true}
	if resetAt != nil {
		w.ResetAt = *resetAt
		w.HasReset = true
	}
	return w
}

func TestApplyGeminiWebQuotaRateLimit(t *testing.T) {
	// IsSchedulable reads the wall clock, so the windows are anchored to it.
	now := time.Now()
	fiveHourReset := now.Add(35 * time.Minute)
	weeklyReset := now.Add(4*24*time.Hour + 17*time.Hour)
	past := now.Add(-time.Minute)

	cases := []struct {
		name      string
		windows   []geminiweb.QuotaWindow
		wantUntil *time.Time
	}{
		{
			name:      "only five hour exhausted",
			windows:   []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 100, &fiveHourReset), geminiWebTestWindow("weekly", 28, &weeklyReset)},
			wantUntil: &fiveHourReset,
		},
		{
			name:      "only weekly exhausted",
			windows:   []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 40, &fiveHourReset), geminiWebTestWindow("weekly", 100, &weeklyReset)},
			wantUntil: &weeklyReset,
		},
		{
			name:      "both exhausted uses later reset",
			windows:   []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 100, &fiveHourReset), geminiWebTestWindow("weekly", 100, &weeklyReset)},
			wantUntil: &weeklyReset,
		},
		{
			name:    "below limit",
			windows: []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 99, &fiveHourReset), geminiWebTestWindow("weekly", 28, &weeklyReset)},
		},
		{
			name:    "exhausted without reset",
			windows: []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 100, nil), geminiWebTestWindow("weekly", 28, &weeklyReset)},
		},
		{
			name:    "exhausted with past reset",
			windows: []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 100, &past), geminiWebTestWindow("weekly", 28, &weeklyReset)},
		},
		{
			name:      "missing reset falls back to other exhausted window",
			windows:   []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 100, nil), geminiWebTestWindow("weekly", 100, &weeklyReset)},
			wantUntil: &weeklyReset,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &geminiWebQuotaRateLimitRepo{}
			account := &Account{ID: 259, Platform: PlatformGemini}
			applyGeminiWebQuotaRateLimit(context.Background(), repo, account, geminiweb.Quota{Windows: tc.windows}, now)
			require.Zero(t, repo.clearCalls)
			if tc.wantUntil == nil {
				require.Empty(t, repo.setCalls)
				require.Nil(t, account.RateLimitResetAt)
				return
			}
			require.Equal(t, []time.Time{*tc.wantUntil}, repo.setCalls)
			require.NotNil(t, account.RateLimitResetAt)
			require.Equal(t, *tc.wantUntil, *account.RateLimitResetAt)
			require.False(t, account.IsSchedulable())
		})
	}
}

func TestApplyGeminiWebQuotaRateLimitKeepsExistingLaterLimit(t *testing.T) {
	now := time.Date(2026, 10, 18, 17, 0, 0, 0, time.UTC)
	fiveHourReset := now.Add(35 * time.Minute)
	limitedAt := now.Add(-time.Hour)
	existing := now.Add(2 * time.Hour)
	repo := &geminiWebQuotaRateLimitRepo{}
	account := &Account{ID: 259, RateLimitedAt: &limitedAt, RateLimitResetAt: &existing}
	quota := geminiweb.Quota{Windows: []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 100, &fiveHourReset)}}

	applyGeminiWebQuotaRateLimit(context.Background(), repo, account, quota, now)

	require.Empty(t, repo.setCalls)
	require.Equal(t, existing, *account.RateLimitResetAt)
}

func TestApplyGeminiWebQuotaRateLimitClearsQuotaLimitWhenUsageFallsBack(t *testing.T) {
	now := time.Date(2026, 10, 18, 17, 0, 0, 0, time.UTC)
	weeklyReset := now.Add(24 * time.Hour)
	quota := geminiweb.Quota{Windows: []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 10, nil), geminiWebTestWindow("weekly", 30, &weeklyReset)}}

	limitedAt := now.Add(-time.Hour)
	quotaLimit := now.Add(3 * time.Hour)
	repo := &geminiWebQuotaRateLimitRepo{}
	account := &Account{ID: 259, RateLimitedAt: &limitedAt, RateLimitResetAt: &quotaLimit}
	applyGeminiWebQuotaRateLimit(context.Background(), repo, account, quota, now)
	require.Equal(t, 1, repo.clearCalls)
	require.Nil(t, account.RateLimitResetAt)

	// A short 429 cooldown is not a quota limit and stays in place.
	cooldownAt := now.Add(-10 * time.Second)
	cooldownUntil := cooldownAt.Add(time.Minute)
	repo = &geminiWebQuotaRateLimitRepo{}
	account = &Account{ID: 259, RateLimitedAt: &cooldownAt, RateLimitResetAt: &cooldownUntil}
	applyGeminiWebQuotaRateLimit(context.Background(), repo, account, quota, now)
	require.Zero(t, repo.clearCalls)
	require.Equal(t, cooldownUntil, *account.RateLimitResetAt)
}

func TestApplyGeminiWebQuotaRateLimitKeepsLimitWhileExhaustedWithoutReset(t *testing.T) {
	now := time.Date(2026, 10, 18, 17, 0, 0, 0, time.UTC)
	limitedAt := now.Add(-time.Hour)
	quotaLimit := now.Add(3 * time.Hour)
	repo := &geminiWebQuotaRateLimitRepo{}
	account := &Account{ID: 259, RateLimitedAt: &limitedAt, RateLimitResetAt: &quotaLimit}
	quota := geminiweb.Quota{Windows: []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 100, nil)}}

	applyGeminiWebQuotaRateLimit(context.Background(), repo, account, quota, now)

	require.Zero(t, repo.clearCalls)
	require.Empty(t, repo.setCalls)
	require.Equal(t, quotaLimit, *account.RateLimitResetAt)
}

type geminiWebQuotaAsyncRepo struct {
	AccountRepository
	rateLimited chan time.Time
}

func (r *geminiWebQuotaAsyncRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	r.rateLimited <- resetAt
	return nil
}

type geminiWebQuotaUpstream struct {
	HTTPUpstream
	fiveHourReset time.Time
	quotaReads    atomic.Int32
}

func (u *geminiWebQuotaUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
	if req.Method == http.MethodPost {
		u.quotaReads.Add(1)
		payload, _ := json.Marshal([]any{2, []any{
			[]any{"", "1", 1, []any{strconv.FormatInt(u.fiveHourReset.Unix(), 10), "0"}},
			[]any{"", "0.28", 2, []any{strconv.FormatInt(u.fiveHourReset.Add(96*time.Hour).Unix(), 10), "0"}},
		}})
		outer, _ := json.Marshal([]any{[]any{"wrb.fr", "jSf9Qc", string(payload)}})
		body = ")]}'\n" + string(outer)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestRefreshGeminiWebQuotaAfterRequestRateLimitsExhaustedAccount(t *testing.T) {
	reset := time.Now().Add(35 * time.Minute).Truncate(time.Second)
	upstream := &geminiWebQuotaUpstream{fiveHourReset: reset}
	repo := &geminiWebQuotaAsyncRepo{rateLimited: make(chan time.Time, 2)}
	svc := &GeminiMessagesCompatService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{ID: 268, Platform: PlatformGemini, Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"}}

	svc.refreshGeminiWebQuotaAfterRequest(account)
	select {
	case got := <-repo.rateLimited:
		require.True(t, got.Equal(reset), "rate limit until %v, want %v", got, reset)
	case <-time.After(5 * time.Second):
		t.Fatal("exhausted five-hour window did not rate limit the account")
	}
	// The request's own account is left alone; the background read used a copy.
	require.Nil(t, account.RateLimitResetAt)

	// A burst right after shares the read that just happened.
	svc.refreshGeminiWebQuotaAfterRequest(account)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, int32(1), upstream.quotaReads.Load())
}

func TestGeminiWebQuotaRefreshGate(t *testing.T) {
	var gate geminiWebQuotaRefreshGate
	now := time.Now()
	require.True(t, gate.allow(1, now))
	require.False(t, gate.allow(1, now.Add(geminiWebQuotaRefreshInterval-time.Second)))
	require.True(t, gate.allow(2, now))
	require.True(t, gate.allow(1, now.Add(geminiWebQuotaRefreshInterval)))
}

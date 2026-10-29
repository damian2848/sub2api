package service

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/stretchr/testify/require"
)

func TestGeminiWebCleanupWindowsRequiresExplicitFreshExhaustion(t *testing.T) {
	now := time.Now().UTC()
	w := geminiweb.QuotaWindow{ID: "five_hour", HasPercent: true, UsedPercent: 100, RemainingPercent: 0, HasReset: true, ResetAt: now.Add(time.Hour)}
	valid := geminiweb.Quota{ObservedAt: now, Windows: []geminiweb.QuotaWindow{w}}
	require.Len(t, geminiWebCleanupWindows(valid, now), 1)
	for _, tc := range []struct {
		name string
		edit func(*geminiweb.Quota)
	}{
		{"missing observation", func(q *geminiweb.Quota) { q.ObservedAt = time.Time{} }},
		{"stale observation", func(q *geminiweb.Quota) { q.ObservedAt = now.Add(-3 * time.Minute) }},
		{"future observation", func(q *geminiweb.Quota) { q.ObservedAt = now.Add(time.Minute) }},
		{"missing percent", func(q *geminiweb.Quota) { q.Windows[0].HasPercent = false }},
		{"remaining quota", func(q *geminiweb.Quota) { q.Windows[0].RemainingPercent = 1 }},
		{"inconsistent percent", func(q *geminiweb.Quota) { q.Windows[0].UsedPercent = 99 }},
		{"negative percent", func(q *geminiweb.Quota) { q.Windows[0].RemainingPercent = -1 }},
		{"nan percent", func(q *geminiweb.Quota) { q.Windows[0].UsedPercent = math.NaN() }},
		{"missing real reset", func(q *geminiweb.Quota) { q.Windows[0].HasReset = false }},
		{"expired window", func(q *geminiweb.Quota) { q.Windows[0].ResetAt = now.Add(-time.Second) }},
		{"implausible reset", func(q *geminiweb.Quota) { q.Windows[0].ResetAt = now.Add(24 * time.Hour) }},
		{"unknown window", func(q *geminiweb.Quota) { q.Windows[0].ID = "monthly" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := geminiweb.Quota{ObservedAt: now, Windows: []geminiweb.QuotaWindow{w}}
			tc.edit(&q)
			require.Empty(t, geminiWebCleanupWindows(q, now))
		})
	}
	weekly := w
	weekly.ID, weekly.ResetAt = "weekly", now.Add(7*24*time.Hour)
	valid.Windows = append(valid.Windows, weekly, w)
	windows := geminiWebCleanupWindows(valid, now)
	require.Len(t, windows, 2)
	require.Equal(t, windows[0].ResetAt, w.ResetAt.Truncate(time.Microsecond))
}

func TestGeminiWebHistoryIdentityTracksLoginButAllowsRotation(t *testing.T) {
	a := &Account{Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=principal; __Secure-1PSIDTS=old", geminiWebCookieGeneration: "one"}}
	identity := geminiWebHistoryIdentity(a)
	require.Len(t, identity, 64)
	a.Credentials["gemini_web_cookie"] = "__Secure-1PSIDTS=new; __Secure-1PSID=principal"
	require.Equal(t, identity, geminiWebHistoryIdentity(a))
	a.Credentials["gemini_web_cookie"] = `[{"name":"__Secure-1PSID","value":"principal","domain":".google.com"}]`
	require.Equal(t, identity, geminiWebHistoryIdentity(a))
	a.Credentials["auth_user"] = "1"
	require.NotEqual(t, identity, geminiWebHistoryIdentity(a))
	delete(a.Credentials, "auth_user")
	a.Credentials[geminiWebCookieGeneration] = "two"
	require.NotEqual(t, identity, geminiWebHistoryIdentity(a))
	a.Credentials["gemini_web_cookie"] = "__Secure-1PSIDTS=missing-principal"
	require.Empty(t, geminiWebHistoryIdentity(a))
}

type historyPolicyRepository struct {
	geminiWebQuotaRateLimitRepo
	GeminiWebHistoryCleanupRepository
	queued   []GeminiWebCleanupWindow
	ctxErr   error
	beginErr error
	released string
}

func (r *historyPolicyRepository) EnqueueGeminiWebHistoryCleanup(ctx context.Context, _ int64, _ string, windows []GeminiWebCleanupWindow) error {
	r.ctxErr = ctx.Err()
	r.queued = windows
	return nil
}
func (r *historyPolicyRepository) BeginGeminiWebHistoryRequest(_ context.Context, _ int64, ttl time.Duration) (string, error) {
	if ttl < 6*time.Minute {
		panic("unsafe request TTL")
	}
	return "request-token", r.beginErr
}
func (r *historyPolicyRepository) EndGeminiWebHistoryRequest(ctx context.Context, _ int64, token string) error {
	r.ctxErr = ctx.Err()
	r.released = token
	return nil
}

func TestApplyQuotaEnqueuesEvenWhenAlreadyRateLimitedAndCallerCancelled(t *testing.T) {
	now, reset := time.Now(), time.Now().Add(time.Hour)
	a := &Account{ID: 42, Platform: PlatformGemini, Type: AccountTypeGeminiWeb, Status: StatusActive, RateLimitResetAt: &reset, Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=principal"}}
	r := &historyPolicyRepository{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	applyGeminiWebQuotaRateLimit(ctx, r, a, geminiweb.Quota{ObservedAt: now, Windows: []geminiweb.QuotaWindow{geminiWebTestWindow("five_hour", 100, &reset)}}, now)
	require.Len(t, r.queued, 1)
	require.NoError(t, r.ctxErr)
	require.Empty(t, r.setCalls)
}

func TestHistoryAdmissionFailsClosedAndReleasesAfterCancellation(t *testing.T) {
	r := &historyPolicyRepository{beginErr: ErrGeminiWebHistoryMaintenance}
	end, err := beginGeminiWebHistoryRequest(context.Background(), r, 42)
	require.True(t, errors.Is(err, ErrGeminiWebHistoryMaintenance))
	require.Nil(t, end)
	r.beginErr = nil
	ctx, cancel := context.WithCancel(context.Background())
	end, err = beginGeminiWebHistoryRequest(ctx, r, 42)
	require.NoError(t, err)
	cancel()
	end()
	require.Equal(t, "request-token", r.released)
	require.NoError(t, r.ctxErr)
}

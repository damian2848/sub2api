package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

// Deletion requires a fresh, complete, explicit upstream exhaustion signal.
// Scheduling may use estimated cooldowns, but they never authorize cleanup.
func geminiWebCleanupWindows(quota geminiweb.Quota, now time.Time) []GeminiWebCleanupWindow {
	if quota.ObservedAt.IsZero() || now.Sub(quota.ObservedAt) > 2*time.Minute || quota.ObservedAt.After(now.Add(5*time.Second)) {
		return nil
	}
	var windows []GeminiWebCleanupWindow
	seen := map[string]bool{}
	for _, w := range quota.Windows {
		if w.ID != "five_hour" && w.ID != "weekly" {
			continue
		}
		maxFuture := 8 * 24 * time.Hour
		if w.ID == "five_hour" {
			maxFuture = 6 * time.Hour
		}
		if seen[w.ID] || !w.HasPercent || w.UsedPercent != 100 || w.RemainingPercent != 0 || !w.HasReset || !w.ResetAt.After(now) || w.ResetAt.After(now.Add(maxFuture)) {
			continue
		}
		seen[w.ID] = true
		windows = append(windows, GeminiWebCleanupWindow{ID: w.ID, ResetAt: w.ResetAt.UTC().Truncate(time.Microsecond)})
	}
	return windows
}

// Bind jobs to the selected Google login, while allowing routine timestamp
// cookie rotations. Never persist authentication material in a cleanup job.
func geminiWebHistoryIdentity(account *Account) string {
	if account == nil {
		return ""
	}
	var psid string
	for _, part := range strings.Split(geminiweb.NormalizeCookieHeader(account.GetCredential("gemini_web_cookie")), ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && kv[0] == "__Secure-1PSID" {
			psid = strings.TrimSpace(kv[1])
		}
	}
	if psid == "" {
		return ""
	}
	authUser, authUserSet := geminiWebAuthUser(account)
	payload, _ := json.Marshal([]any{psid, authUser, authUserSet, account.GetCredential(geminiWebCookieGeneration)})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func observeGeminiWebHistoryQuota(ctx context.Context, repo AccountRepository, account *Account, quota geminiweb.Quota, now time.Time) {
	jobs, ok := repo.(GeminiWebHistoryCleanupRepository)
	if !ok || account == nil || account.ID <= 0 || !account.IsGeminiWeb() || account.Status != StatusActive {
		return
	}
	windows := geminiWebCleanupWindows(quota, now)
	identity := geminiWebHistoryIdentity(account)
	if len(windows) == 0 || identity == "" {
		return
	}
	if err := jobs.EnqueueGeminiWebHistoryCleanup(ctx, account.ID, identity, windows); err != nil {
		slog.Warn("gemini_web_history_cleanup_enqueue_failed", "account_id", account.ID)
	}
}

// Generation is bounded to five minutes at both callers. The extra minute
// covers cancellation and process crashes before the durable request expires.
func beginGeminiWebHistoryRequest(ctx context.Context, repo AccountRepository, accountID int64) (func(), error) {
	jobs, ok := repo.(GeminiWebHistoryCleanupRepository)
	if !ok {
		return func() {}, nil
	}
	token, err := jobs.BeginGeminiWebHistoryRequest(ctx, accountID, 6*time.Minute)
	if err != nil {
		return nil, err
	}
	return func() {
		endCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := jobs.EndGeminiWebHistoryRequest(endCtx, accountID, token); err != nil {
			slog.Warn("gemini_web_history_request_release_failed", "account_id", accountID)
		}
	}, nil
}

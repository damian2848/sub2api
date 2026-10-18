package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// A single canned failure reply is usually a one-off and only switches
// accounts. Some accounts degrade and return them repeatedly (one returned 12
// of 28 replies in four hours, six within ten minutes), and sticky sessions
// keep sending the next turn back to them. Such an account is taken out of
// scheduling for a while.
const (
	geminiWebCannedCooldownThreshold = 3
	geminiWebCannedCooldownWindow    = 10 * time.Minute
	geminiWebCannedCooldownDuration  = 10 * time.Minute
)

const geminiWebCannedCooldownReason = "Gemini Web returned canned failure replies repeatedly"

// geminiWebCannedTracker counts recent canned replies per account. It is
// process-local, like the other scheduling counters; the cooldown itself is
// stored on the account.
type geminiWebCannedTracker struct {
	mu   sync.Mutex
	hits map[int64][]time.Time
}

// record notes a canned reply and reports whether the account reached the
// cooldown threshold. Reaching it resets the account's count.
func (t *geminiWebCannedTracker) record(accountID int64, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.hits == nil {
		t.hits = make(map[int64][]time.Time)
	}
	cutoff := now.Add(-geminiWebCannedCooldownWindow)
	recent := t.hits[accountID][:0]
	for _, at := range t.hits[accountID] {
		if at.After(cutoff) {
			recent = append(recent, at)
		}
	}
	recent = append(recent, now)
	if len(recent) >= geminiWebCannedCooldownThreshold {
		delete(t.hits, accountID)
		return true
	}
	t.hits[accountID] = recent
	return false
}

// checkGeminiWebCannedReply is geminiWebCheckCannedReply for gateway traffic:
// it also counts the reply towards the account's cooldown.
func (s *GeminiMessagesCompatService) checkGeminiWebCannedReply(ctx context.Context, account *Account, result geminiweb.Result, canned geminiweb.CannedReplies) error {
	err := geminiWebCheckCannedReply(ctx, account, result, canned)
	if err != nil {
		s.recordGeminiWebCannedReply(ctx, account)
	}
	return err
}

func (s *GeminiMessagesCompatService) recordGeminiWebCannedReply(ctx context.Context, account *Account) {
	if s == nil || account == nil || account.ID <= 0 {
		return
	}
	now := time.Now()
	if !s.geminiWebCanned.record(account.ID, now) || s.accountRepo == nil {
		return
	}
	until := now.Add(geminiWebCannedCooldownDuration)
	// The request may already be failing over or cancelled; the cooldown
	// must still be stored.
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := s.accountRepo.SetTempUnschedulable(storeCtx, account.ID, until, geminiWebCannedCooldownReason); err != nil {
		logger.FromContext(ctx).Warn("gemini_web.canned_reply_cooldown_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return
	}
	account.TempUnschedulableUntil = &until
	recordGeminiWebCooldown(ctx, account, until)
	logger.FromContext(ctx).Warn("gemini_web.canned_reply_cooldown",
		zap.Int64("account_id", account.ID),
		zap.Int("canned_replies", geminiWebCannedCooldownThreshold),
		zap.Duration("window", geminiWebCannedCooldownWindow),
		zap.Time("until", until),
	)
}

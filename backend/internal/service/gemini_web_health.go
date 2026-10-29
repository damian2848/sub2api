package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const (
	geminiWebHealthFailureThreshold = 3
	geminiWebHealthFailureWindow    = time.Minute
	geminiWebHealthInitialCooldown  = time.Minute
	geminiWebHealthMaxCooldown      = 10 * time.Minute
	geminiWebHealthCooldownReason   = "Gemini Web repeated upstream failures"
)

var errGeminiWebHealthCooling = errors.New("gemini web account is cooling down or awaiting a recovery probe")

type geminiWebHealthOutcome uint8

const (
	geminiWebHealthNeutral geminiWebHealthOutcome = iota
	geminiWebHealthSuccess
	geminiWebHealthFailure
)

type geminiWebHealthState struct {
	generation           uint64
	credentialGeneration string
	failures             int
	firstFailure         time.Time
	level                int
	until                time.Time
	observedUntil        time.Time
	probing              bool
}

type geminiWebHealthAttempt struct {
	accountID            int64
	generation           uint64
	credentialGeneration string
	probe                bool
}

type geminiWebHealthCooldown struct {
	until time.Time
	level int
}

// Counters and probe admission are process-local, like the canned tracker.
// The open interval is persisted on the account and synchronously propagated
// by the generation-aware repository setter to the shared scheduler snapshot.
// A persisted interval also restores half-open admission after a restart.
type geminiWebHealthTracker struct {
	mu     sync.Mutex
	states map[int64]*geminiWebHealthState
}

func geminiWebHealthLevel(reason string) int {
	if !strings.HasPrefix(reason, geminiWebHealthCooldownReason) {
		return 0
	}
	level, _ := strconv.Atoi(strings.TrimPrefix(reason, geminiWebHealthCooldownReason+"; level="))
	return max(1, min(level, 5))
}

func (t *geminiWebHealthTracker) begin(account *Account, now time.Time) (geminiWebHealthAttempt, bool) {
	if account == nil || account.ID <= 0 {
		return geminiWebHealthAttempt{}, true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.states == nil {
		t.states = make(map[int64]*geminiWebHealthState)
	}
	state := t.states[account.ID]
	credentialGeneration := account.GetCredential(geminiWebCookieGeneration)
	if state == nil || state.credentialGeneration != credentialGeneration {
		state = &geminiWebHealthState{generation: 1, credentialGeneration: credentialGeneration}
		if previous := t.states[account.ID]; previous != nil {
			state.generation = previous.generation + 1
		}
		t.states[account.ID] = state
	}
	// Never re-open from the stale account snapshot that preceded a successful
	// recovery. A later persisted interval from another worker still wins.
	if level := geminiWebHealthLevel(account.TempUnschedulableReason); level > 0 &&
		account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(state.observedUntil) {
		state.until = *account.TempUnschedulableUntil
		state.observedUntil = state.until
		state.level = level
		state.generation++
		state.probing = false
	}
	if now.Before(state.until) || state.probing {
		return geminiWebHealthAttempt{}, false
	}
	attempt := geminiWebHealthAttempt{
		accountID: account.ID, generation: state.generation,
		credentialGeneration: credentialGeneration, probe: state.level > 0,
	}
	if attempt.probe {
		state.probing = true
	}
	return attempt, true
}

func (t *geminiWebHealthTracker) finish(attempt geminiWebHealthAttempt, outcome geminiWebHealthOutcome, now time.Time) geminiWebHealthCooldown {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.states[attempt.accountID]
	if state == nil || state.generation != attempt.generation {
		// Requests already in flight when the circuit opened cannot undo its
		// cooldown or count against its eventual recovery probe.
		return geminiWebHealthCooldown{}
	}
	if attempt.probe {
		state.probing = false
	}
	if outcome == geminiWebHealthNeutral {
		if attempt.probe {
			// Invalidate the released token so a deferred cleanup cannot release
			// another request's subsequently admitted recovery probe.
			state.generation++
		}
		return geminiWebHealthCooldown{}
	}
	if outcome == geminiWebHealthSuccess {
		state.failures = 0
		state.firstFailure = time.Time{}
		state.level = 0
		state.until = time.Time{}
		if attempt.probe {
			state.generation++
		}
		return geminiWebHealthCooldown{}
	}
	if state.failures == 0 || now.Sub(state.firstFailure) > geminiWebHealthFailureWindow {
		state.failures = 0
		state.firstFailure = now
	}
	state.failures++
	if !attempt.probe && state.failures < geminiWebHealthFailureThreshold {
		return geminiWebHealthCooldown{}
	}
	state.level = min(state.level+1, 5)
	duration := min(geminiWebHealthInitialCooldown*time.Duration(1<<(state.level-1)), geminiWebHealthMaxCooldown)
	state.until = now.Add(duration)
	state.observedUntil = state.until
	state.generation++
	state.failures = 0
	return geminiWebHealthCooldown{until: state.until, level: state.level}
}

func geminiWebClassifyHealth(ctx context.Context, result geminiweb.Result, err error, canned geminiweb.CannedReplies) geminiWebHealthOutcome {
	// Disconnected clients, downstream write failures and their cancellations
	// say nothing about account health.
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return geminiWebHealthNeutral
	}
	if errors.Is(err, geminiweb.ErrEmptyResponse) {
		return geminiWebHealthFailure
	}
	var rpc *geminiweb.UpstreamRPCError
	if errors.As(err, &rpc) {
		switch rpc.BardCode {
		case 1016, 1037, 1013, 1050, 1052, 1060:
			// These known Bard errors already had separate handling before
			// structured RPC parsing; keep their health outcome unchanged.
			return geminiWebHealthNeutral
		default:
			// Unknown RPC failures, including unrecognized detail types,
			// previously became empty responses. Preserve their bounded
			// cooldown without assuming invalid credentials or quota loss.
			return geminiWebHealthFailure
		}
	}
	var upstream *geminiweb.UpstreamHTTPError
	if errors.As(err, &upstream) && upstream.StatusCode >= http.StatusInternalServerError && upstream.StatusCode <= 599 {
		return geminiWebHealthFailure
	}
	if err == nil && !result.GenerationFailed && !canned.Is(result.Text) {
		// Policy refusals are valid upstream answers, not account failures.
		return geminiWebHealthSuccess
	}
	return geminiWebHealthNeutral
}

func (s *GeminiMessagesCompatService) finishGeminiWebHealth(ctx context.Context, account *Account, attempt geminiWebHealthAttempt, result geminiweb.Result, err error) {
	outcome := geminiWebClassifyHealth(ctx, result, err, s.geminiWebCannedReplies(ctx))
	cooldown := s.geminiWebHealth.finish(attempt, outcome, time.Now())
	if cooldown.until.IsZero() || account == nil {
		return
	}
	reason := fmt.Sprintf("%s; level=%d", geminiWebHealthCooldownReason, cooldown.level)
	applied := s.accountRepo == nil
	if s.accountRepo != nil {
		// Do not lose the isolation if this request disconnects while failing over.
		storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		var storeErr error
		if setter, ok := s.accountRepo.(interface {
			SetGeminiWebTempUnschedulable(context.Context, int64, string, time.Time, string) (bool, error)
		}); ok {
			// Capture the credential generation at admission, not completion:
			// an old in-flight failure must never isolate a recovered cookie.
			applied, storeErr = setter.SetGeminiWebTempUnschedulable(storeCtx, attempt.accountID,
				attempt.credentialGeneration, cooldown.until, reason)
		} else {
			storeErr = errors.New("repository does not support generation-aware Gemini Web cooldowns")
		}
		if storeErr != nil {
			logger.FromContext(ctx).Warn("gemini_web.health_cooldown_store_failed",
				zap.Int64("account_id", account.ID), zap.Error(storeErr))
		}
	}
	if applied && account.GetCredential(geminiWebCookieGeneration) == attempt.credentialGeneration &&
		(account.TempUnschedulableUntil == nil || account.TempUnschedulableUntil.Before(cooldown.until)) {
		account.TempUnschedulableUntil = &cooldown.until
		account.TempUnschedulableReason = reason
	}
	logger.FromContext(ctx).Warn("gemini_web.health_cooldown",
		zap.Int64("account_id", account.ID), zap.Int("level", cooldown.level),
		zap.Time("until", cooldown.until), zap.Error(err))
}

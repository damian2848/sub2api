package service

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Opt in by group so an operator can compare the existing and recovery paths.
// Removing the setting restores the previous scheduling and retry behavior.
const antigravityPoolRecoveryGroupsEnv = "ANTIGRAVITY_POOL_RECOVERY_GROUP_IDS"

func antigravityPoolRecoveryEnabled(groupID int64, model string) bool {
	if groupID <= 0 || !strings.HasPrefix(model, "gemini-") {
		return false
	}
	for value := range strings.SplitSeq(os.Getenv(antigravityPoolRecoveryGroupsEnv), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err == nil && id == groupID {
			return true
		}
	}
	return false
}

// Only the exact generic response is evidence for this conservative retry cap.
// Explicit quota, RetryInfo, and other diagnostic details keep their policies.
func isAntigravityGeneric429(status int, headers http.Header, body []byte) bool {
	if status != http.StatusTooManyRequests || headers.Get("Retry-After") != "" {
		return false
	}
	var payload struct {
		Error struct {
			Code    int               `json:"code"`
			Status  string            `json:"status"`
			Message string            `json:"message"`
			Details []json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	return payload.Error.Code == 429 && payload.Error.Status == "RESOURCE_EXHAUSTED" &&
		strings.TrimSpace(payload.Error.Message) == "Resource has been exhausted (e.g. check quota)." && len(payload.Error.Details) == 0
}

func (p antigravityRetryLoopParams) repeatedGeneric429Error(resp *http.Response) *UpstreamFailoverError {
	if p.retryBudget == nil || p.account == nil || p.account.Platform != PlatformAntigravity {
		return nil
	}
	groupID := p.groupID
	if groupID == 0 {
		if group, ok := p.ctx.Value(ctxkey.Group).(*Group); ok && group != nil {
			groupID = group.ID
		}
	}
	if !antigravityPoolRecoveryEnabled(groupID, p.requestedModel) {
		return nil
	}
	b := p.retryBudget
	if resp == nil || !isAntigravityGeneric429(resp.StatusCode, resp.Header, b.body) {
		b.generic429Accounts = nil
		return nil
	}
	if b.generic429Accounts == nil || b.generic429Model != p.requestedModel {
		b.generic429Model = p.requestedModel
		b.generic429Accounts = make(map[int64]struct{})
	}
	b.generic429Accounts[p.account.ID] = struct{}{}
	if len(b.generic429Accounts) < 2 {
		return nil
	}
	// The normal error path has already recorded this account's model cooldown.
	// Stop this request, without marking any untried account or other model.
	logger.FromContext(p.ctx).Warn("gateway.antigravity_generic_429_budget_exhausted",
		zap.Int64("group_id", groupID), zap.String("model", p.requestedModel),
		zap.Int64("account_id", p.account.ID), zap.Int("failed_accounts", len(b.generic429Accounts)))
	failure := b.exhaustedError()
	failure.Reason = "antigravity_repeated_generic_429"
	return failure
}

type antigravityPoolWaitKey struct {
	groupID int64
	model   string
}

var antigravityPoolWaiters = struct {
	sync.Mutex
	counts map[antigravityPoolWaitKey]int
	total  int
}{counts: make(map[antigravityPoolWaitKey]int)}

// This waits once, only for a proven all-account cooldown near its end. It does
// not clear cooldowns, reserve an account, or start an upstream request early.
// The original selector rechecks all eligibility and acquires a slot afterward.
func (s *GatewayService) SelectAccountWithLoadAwareness(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, metadataUserID string, sub2apiUserID int64) (*AccountSelectionResult, error) {
	selection, selectErr := s.selectAccountWithLoadAwareness(ctx, groupID, sessionHash, requestedModel, excludedIDs, metadataUserID, sub2apiUserID)
	if selection != nil || !errors.Is(selectErr, ErrNoAvailableAccounts) || len(excludedIDs) > 0 || ctx.Err() != nil || geminiCachedContentBoundFromContext(ctx) || groupID == nil ||
		!antigravityPoolRecoveryEnabled(*groupID, requestedModel) {
		return selection, selectErr
	}
	group, resolvedID, err := s.checkClaudeCodeRestriction(ctx, groupID)
	if err != nil || resolvedID == nil || *resolvedID != *groupID {
		return selection, selectErr
	}
	platform, _ := ctx.Value(ctxkey.ForcePlatform).(string)
	if platform == "" && group != nil {
		platform = group.Platform
	}
	if platform != PlatformAntigravity {
		return selection, selectErr
	}
	diagnosis := s.DiagnoseModelAvailabilityForPlatform(ctx, groupID, requestedModel, platform)
	if !diagnosis.AllModelAccountsRateLimited() {
		return selection, selectErr
	}
	delay := time.Until(*diagnosis.EarliestRateLimitResetAt) + time.Duration(100+rand.Intn(200))*time.Millisecond
	if delay <= 0 || delay > 5*time.Second {
		return selection, selectErr
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
		return selection, selectErr
	}
	key := antigravityPoolWaitKey{groupID: *groupID, model: requestedModel}
	antigravityPoolWaiters.Lock()
	if antigravityPoolWaiters.counts[key] >= 64 || antigravityPoolWaiters.total >= 128 {
		antigravityPoolWaiters.Unlock()
		return selection, selectErr
	}
	antigravityPoolWaiters.counts[key]++
	antigravityPoolWaiters.total++
	antigravityPoolWaiters.Unlock()
	defer func() {
		antigravityPoolWaiters.Lock()
		antigravityPoolWaiters.counts[key]--
		antigravityPoolWaiters.total--
		if antigravityPoolWaiters.counts[key] == 0 {
			delete(antigravityPoolWaiters.counts, key)
		}
		antigravityPoolWaiters.Unlock()
	}()
	logger.FromContext(ctx).Info("gateway.antigravity_pool_wait",
		zap.Int64("group_id", *groupID), zap.String("model", requestedModel), zap.Duration("wait", delay))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	selection, selectErr = s.selectAccountWithLoadAwareness(ctx, groupID, sessionHash, requestedModel, excludedIDs, metadataUserID, sub2apiUserID)
	logger.FromContext(ctx).Info("gateway.antigravity_pool_reselect",
		zap.Int64("group_id", *groupID), zap.String("model", requestedModel), zap.Bool("selected", selection != nil && selectErr == nil))
	return selection, selectErr
}

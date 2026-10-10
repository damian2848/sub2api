package service

import (
	"context"
	"log/slog"
	"sort"
)

type gatewayPriorityFailbackAttemptedKey struct{}

// Shared by Claude, Gemini, Antigravity and composite/compatible groups.
// Metadata callers keep their slot-free contract; generation callers acquire
// capacity before committing a replacement binding.
func (s *GatewayService) tryPriorityStickyFailback(ctx context.Context, groupID *int64, platform string, forcePlatform bool, sessionHash, model string, excluded map[int64]struct{}, stickyID int64, acquire bool, providerChecks ...func(context.Context, *Account) bool) *AccountSelectionResult {
	if s == nil || s.concurrencyService == nil || sessionHash == "" || stickyID <= 0 {
		return nil
	}
	if _, skip := ctx.Value(gatewayPriorityFailbackAttemptedKey{}).(bool); skip {
		return nil
	}
	if _, skip := excluded[stickyID]; skip {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, priorityFailbackTimeout)
	defer cancel()
	accounts, mixed, err := s.listSchedulableAccounts(probeCtx, groupID, platform, forcePlatform)
	if err != nil {
		return nil
	}
	var sticky *Account
	for i := range accounts {
		if accounts[i].ID == stickyID {
			sticky = &accounts[i]
			break
		}
	}
	if sticky == nil {
		return nil
	}
	routed := s.routingAccountIDsForRequest(probeCtx, groupID, model, platform)
	// A model route is an explicit pool boundary, including when it supersedes
	// an old session binding. Leave route fallback to the existing selector.
	if len(routed) > 0 && !containsInt64(routed, stickyID) {
		return nil
	}
	// Most sessions already use the first priority. Do not read window costs,
	// RPM or loads unless a higher-priority candidate actually exists.
	higher := make([]Account, 0, len(accounts))
	for _, a := range accounts {
		if a.Priority >= sticky.Priority || a.ID == stickyID {
			continue
		}
		if _, skip := excluded[a.ID]; skip {
			continue
		}
		if len(routed) == 0 || containsInt64(routed, a.ID) {
			higher = append(higher, a)
		}
	}
	if len(higher) == 0 {
		return nil
	}
	accounts = higher
	var group *Group
	if groupID != nil {
		if s.groupRepo == nil && s.schedulerSnapshot == nil && s.groupFromContext(probeCtx, *groupID) == nil {
			return nil
		}
		group, err = s.resolveProfitControlGroup(probeCtx, *groupID)
		if err != nil || group == nil {
			return nil
		}
	}
	probeCtx = s.withWindowCostPrefetch(probeCtx, accounts)
	probeCtx, err = withAccountRPMPrefetch(probeCtx, s.rpmCache, accounts, PlatformAnthropic)
	if err != nil {
		return nil
	}
	eligible := func(a *Account) bool {
		if a == nil || a.Priority >= sticky.Priority || a.ID == sticky.ID ||
			!openAIStickyAccountMatchesGroup(a, groupID) ||
			!s.isAccountAllowedForPlatform(a, platform, mixed) ||
			!s.isAccountSchedulableForSelection(a) || s.isAccountBlockedBySchedulingThreshold(probeCtx, a) ||
			!s.isGatewayAccountProfitEligible(probeCtx, a) ||
			group != nil && group.RequirePrivacySet && !a.IsPrivacySet() {
			return false
		}
		for _, check := range providerChecks {
			if !check(probeCtx, a) {
				return false
			}
		}
		if _, skip := excluded[a.ID]; skip {
			return false
		}
		if len(routed) > 0 && !containsInt64(routed, a.ID) {
			return false
		}
		if model != "" && !s.isModelSupportedByAccountInGroup(probeCtx, a, groupID, model) ||
			!s.isAccountSchedulableForModelSelection(probeCtx, a, model) ||
			!s.isAccountSchedulableForQuota(a) ||
			!s.isAccountSchedulableForWindowCost(probeCtx, a, false) ||
			s.isStickyAccountUpstreamRestricted(probeCtx, groupID, a, model) {
			return false
		}
		rpm, rpmErr := readAccountRPMState(probeCtx, s.rpmCache, a)
		return rpmErr == nil && (!rpm.Enabled || rpm.Utilization() < accountRPMWarningRatio)
	}
	pool := make([]*Account, 0, len(accounts))
	loads := make([]AccountWithConcurrency, 0, len(accounts))
	for i := range accounts {
		a := &accounts[i]
		if eligible(a) {
			pool = append(pool, a)
			loads = append(loads, AccountWithConcurrency{ID: a.ID, MaxConcurrency: a.EffectiveLoadFactor()})
		}
	}
	if len(pool) == 0 {
		return nil
	}
	loadMap, err := s.concurrencyService.GetAccountsLoadBatch(probeCtx, loads)
	if err != nil {
		return nil
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].Priority != pool[j].Priority {
			return pool[i].Priority < pool[j].Priority
		}
		a, b := loadMap[pool[i].ID], loadMap[pool[j].ID]
		if a == nil || b == nil {
			return a != nil
		}
		return a.LoadRate < b.LoadRate
	})
	for _, a := range pool {
		if !priorityFailbackHasHeadroom(a, loadMap[a.ID]) || probeCtx.Err() != nil {
			continue
		}
		var release func()
		if acquire {
			result, acquireErr := s.tryAcquireAccountSlot(probeCtx, a.ID, a.Concurrency)
			if acquireErr != nil || result == nil || !result.Acquired {
				continue
			}
			release = result.ReleaseFunc
		}
		// Re-read after acquisition: priority, membership, model access and
		// concurrency can all change after the candidate snapshot was built.
		fresh, freshErr := s.accountRepo.GetByID(probeCtx, a.ID)
		if freshErr != nil || !eligible(fresh) || fresh.Concurrency != a.Concurrency || probeCtx.Err() != nil {
			if release != nil {
				release()
			}
			continue
		}
		if acquire && !s.checkAndRegisterSession(probeCtx, fresh, sessionHash) {
			if release != nil {
				release()
			}
			continue
		}
		_ = s.bindGatewayStickySessionDuringSelection(ctx, groupID, sessionHash, fresh.ID)
		slog.Info("priority_sticky_failback", "platform", platform, "from_account_id", sticky.ID,
			"to_account_id", fresh.ID, "from_priority", sticky.Priority, "to_priority", fresh.Priority)
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{Account: fresh, Acquired: acquire, ReleaseFunc: release, priorityFailback: true})
	}
	return nil
}

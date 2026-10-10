package service

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Fail back a movable session only after acquiring a healthy
// higher-priority account. Failed reads/slot races leave the old binding usable.
// This is independent of the optional advanced/experimental scheduler switches.
func (s *defaultOpenAIAccountScheduler) tryPriorityStickyFailback(ctx context.Context, req OpenAIAccountScheduleRequest, sticky *Account, acquire bool) *AccountSelectionResult {
	if s == nil || s.service == nil || s.service.concurrencyService == nil || sticky == nil || req.SessionHash == "" ||
		req.DisableStickyEscape || req.PreserveStickyBinding || strings.TrimSpace(req.PreviousResponseID) != "" ||
		preserveOpenAIGuardianParentBinding(ctx, req.SessionHash) {
		return nil
	}
	// Failback is optional work on a healthy session; bound its dependency cost.
	probeCtx, cancel := context.WithTimeout(ctx, priorityFailbackTimeout)
	defer cancel()
	accounts, err := s.service.listSchedulableAccountsForRequest(probeCtx, req.GroupID, req.Platform, req.RequestedModel, req.RequireCompact, req.ExcludedIDs)
	if err != nil {
		return nil
	}
	cfg := s.service.prioritySchedulingRuntimeConfig()
	priorityEnabled := req.Platform == PlatformOpenAI && req.UseUpstreamTokenCost && req.RequiredImageCapability == "" && cfg.applies(req.GroupID, req.RequestedModel)
	escapeCfg := s.service.openAIStickyEscapeConfig()
	balanceProtocols := s.service.balancesPriorityProtocols(req)
	pool := make([]openAIAccountCandidateScore, 0, len(accounts))
	for i := range accounts {
		a := &accounts[i]
		if a.ID == sticky.ID ||
			!isOpenAICompatibleAccountEligibleForRequest(probeCtx, a, req.GroupID, req.Platform, req.RequestedModel, req.RequireCompact, req.RequiredCapability) ||
			!s.service.openAIAccountMatchesSchedulingGroup(a, req.GroupID) ||
			!s.isAccountRequestCompatible(probeCtx, a, req) || !s.isAccountTransportCompatible(a, req.RequiredTransport, req.RequestedModel) ||
			s.service.isExcelBPSCoolingDown(a, req.RequestedModel) {
			continue
		}
		if _, excluded := req.ExcludedIDs[a.ID]; excluded {
			continue
		}
		// Let the normal protocol selection retire a native sticky binding
		// when BPS is explicitly preferred, regardless of account priority.
		if !balanceProtocols && !sticky.IsExcelBPSEnabledForModel(req.RequestedModel) && a.IsExcelBPSEnabledForModel(req.RequestedModel) {
			return nil
		}
		if a.Priority >= sticky.Priority {
			continue
		}
		// Preserve explicit protocol/subscription/compact preferences.
		if !balanceProtocols && sticky.IsExcelBPSEnabledForModel(req.RequestedModel) && !a.IsExcelBPSEnabledForModel(req.RequestedModel) ||
			req.SubscriptionPriority && sticky.IsOpenAIChatGPTSubscription() && !a.IsOpenAIChatGPTSubscription() ||
			req.RequireCompact && openAICompactSupportTier(a) < openAICompactSupportTier(sticky) {
			continue
		}
		item := openAIAccountCandidateScore{account: a}
		if s.stats != nil {
			item.errorRate, item.ttft, item.hasTTFT = s.stats.snapshot(a.ID)
		}
		if item.errorRate > 0.2 || escapeCfg.enabled && item.hasTTFT && item.ttft > escapeCfg.ttftMs {
			continue
		}
		pool = append(pool, item)
	}
	if len(pool) == 0 {
		return nil
	}
	rpmAccounts := make([]Account, 0, len(pool))
	loads := make([]AccountWithConcurrency, 0, len(pool))
	for _, item := range pool {
		rpmAccounts = append(rpmAccounts, *item.account)
		loads = append(loads, AccountWithConcurrency{ID: item.account.ID, MaxConcurrency: item.account.EffectiveLoadFactor()})
	}
	probeCtx, err = s.service.withOpenAIRPMPrefetch(probeCtx, rpmAccounts)
	if err != nil {
		return nil
	}
	loadMap, err := s.service.concurrencyService.GetAccountsLoadBatch(probeCtx, loads)
	if err != nil {
		return nil
	}
	history := priorityHistoryResult{}
	if priorityEnabled {
		history = s.service.cachedPriorityHistory(req, cfg, pool)
	}
	now := time.Now()
	eligible := pool[:0]
	for _, item := range pool {
		a, load := item.account, loadMap[item.account.ID]
		// Leave headroom before moving a warm session; do not oscillate at full
		// capacity or move it to a queue based on a stale optimistic snapshot.
		if !priorityFailbackHasHeadroom(a, load) {
			continue
		}
		item.loadInfo, item.loadKnown = load, true
		if rpm, ok := accountRPMStateFromContext(probeCtx, a); ok && rpm.Enabled {
			if a.CheckRPMSchedulability(rpm.Current) != WindowCostSchedulable || rpm.Current*100 >= rpm.Limit*80 {
				continue
			}
		}
		if priorityEnabled {
			score := applyPriorityCandidate(cfg, &item, history.signals[a.ID], now)
			if item.priorityUnhealthy || score.Tier == "degraded" {
				continue
			}
		}
		eligible = append(eligible, item)
	}
	if len(eligible) == 0 || probeCtx.Err() != nil {
		return nil
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.account.Priority != b.account.Priority {
			return a.account.Priority < b.account.Priority
		}
		return openAIRPMEffectiveLoad(probeCtx, a.account, a.loadInfo.LoadRate) < openAIRPMEffectiveLoad(probeCtx, b.account, b.loadInfo.LoadRate)
	})
	// Commit the new binding only on successful acquisition and all fresh
	// checks. Profit-controlled bindings still commit at terminal admission.
	probeReq := req
	probeReq.PreserveStickyBinding = true
	var selection *AccountSelectionResult
	if acquire {
		selection, _, err = s.tryAcquireOpenAISelectionOrder(probeCtx, probeReq, eligible)
	} else {
		// Token-count and metadata selectors do not reserve generation slots.
		// They still need known headroom and fresh compatibility before rebinding.
		for _, item := range eligible {
			fresh, freshErr := s.service.getSchedulableAccount(probeCtx, item.account.ID)
			fresh = s.service.resolveFreshSchedulableOpenAIAccount(probeCtx, fresh, req.GroupID, req.Platform, req.RequestedModel, req.RequireCompact, req.RequiredCapability)
			if freshErr != nil || fresh == nil || fresh.Priority >= sticky.Priority || fresh.Concurrency != item.account.Concurrency ||
				!s.service.openAIAccountMatchesSchedulingGroup(fresh, req.GroupID) ||
				!s.isAccountRequestCompatible(probeCtx, fresh, req) || !s.isAccountTransportCompatible(fresh, req.RequiredTransport, req.RequestedModel) {
				continue
			}
			selection = attachSelectionProfitGate(ctx, &AccountSelectionResult{Account: fresh})
			break
		}
	}
	if err != nil || selection == nil {
		return nil
	}
	// Without a scheduler snapshot, the shared acquisition path trusts the
	// list query. A binding change needs a fresh read even in that mode.
	if acquire && s.service.schedulerSnapshot == nil {
		fresh, freshErr := s.service.getSchedulableAccount(probeCtx, selection.Account.ID)
		fresh = s.service.resolveFreshSchedulableOpenAIAccount(probeCtx, fresh, req.GroupID, req.Platform, req.RequestedModel, req.RequireCompact, req.RequiredCapability)
		if freshErr != nil || fresh == nil || fresh.Concurrency != selection.Account.Concurrency ||
			!s.service.openAIAccountMatchesSchedulingGroup(fresh, req.GroupID) ||
			!s.isAccountRequestCompatible(probeCtx, fresh, req) || !s.isAccountTransportCompatible(fresh, req.RequiredTransport, req.RequestedModel) {
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			return nil
		}
		selection.Account = fresh
	}
	if probeCtx.Err() != nil || selection.Account.Priority >= sticky.Priority {
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return nil
	}
	_ = s.service.bindOpenAIStickySessionDuringSelection(ctx, req.GroupID, req.SessionHash, selection.Account.ID)
	selection.priorityFailback = true
	slog.Info("priority_sticky_failback", "from_account_id", sticky.ID, "to_account_id", selection.Account.ID,
		"from_priority", sticky.Priority, "to_priority", selection.Account.Priority)
	return selection
}

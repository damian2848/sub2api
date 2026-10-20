package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// ModelAvailabilityDiagnosis describes whether the requested model can be
// served by any persistently eligible account in the group (active with its
// schedulable setting enabled), ignoring transient state such as rate limits,
// overload, temporary unschedulability, and runtime blocks. An unexpired
// cooldown recording an explicit model capability rejection is an exception.
// Handlers use this
// on the "no available accounts" error path to distinguish 404
// model_not_found from 503 service_unavailable.
type ModelAvailabilityDiagnosis struct {
	// HasAccountsInPool is true if the group has at least one persistently
	// eligible account on the queried platform (or, for Anthropic/Gemini, on
	// the platform plus mixed-scheduled Antigravity accounts).
	HasAccountsInPool bool
	// HasModelSupport is true if at least one account's model mapping admits
	// the requested model without an active upstream capability rejection.
	HasModelSupport bool
	// HasRetiredSubscriptionModel records a ChatGPT upstream blocked by model
	// retirement, so an unsupported request can include migration guidance.
	HasRetiredSubscriptionModel bool

	// Complete is false when the authority could not be queried. Counts must
	// never be interpreted as an empty group in that case.
	Complete                 bool
	ConfiguredAccounts       int
	ModelSupportedAccounts   int
	ModelRateLimitedAccounts int
	EarliestRateLimitResetAt *time.Time
}

// AllModelAccountsRateLimited only reports a proven persisted rate-limit
// bottleneck, not inference from the scheduler's reduced candidate pool.
func (d ModelAvailabilityDiagnosis) AllModelAccountsRateLimited() bool {
	return d.Complete && d.ModelSupportedAccounts > 0 &&
		d.ModelRateLimitedAccounts == d.ModelSupportedAccounts && d.EarliestRateLimitResetAt != nil
}

func (d *ModelAvailabilityDiagnosis) recordAccount(ctx context.Context, account *Account, requestedModel string, supported bool, now time.Time) {
	d.HasAccountsInPool = true
	d.ConfiguredAccounts++
	if !supported {
		return
	}
	d.HasModelSupport = true
	d.ModelSupportedAccounts++
	resetAt := modelAvailabilityRateLimitResetAt(ctx, account, requestedModel, now)
	if resetAt == nil {
		return
	}
	d.ModelRateLimitedAccounts++
	if d.EarliestRateLimitResetAt == nil || resetAt.Before(*d.EarliestRateLimitResetAt) {
		d.EarliestRateLimitResetAt = resetAt
	}
}

// An account can recover only after both its global cooldown and all model
// scopes used by selection have expired. Overages can bypass model cooldowns,
// but cannot bypass a global cooldown.
func modelAvailabilityRateLimitResetAt(ctx context.Context, account *Account, requestedModel string, now time.Time) *time.Time {
	var resetAt, modelResetAt *time.Time
	if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(now) {
		resetAt = account.RateLimitResetAt
	}
	for _, key := range account.modelRateLimitKeysForRequest(ctx, requestedModel) {
		if candidate := account.modelRateLimitResetAt(key); candidate != nil && candidate.After(now) &&
			(modelResetAt == nil || candidate.After(*modelResetAt)) {
			modelResetAt = candidate
		}
	}
	if account.Platform == PlatformAntigravity && account.IsOveragesEnabled() {
		creditsResetAt := account.modelRateLimitResetAt(creditsExhaustedKey)
		if creditsResetAt == nil || !creditsResetAt.After(now) {
			modelResetAt = nil
		} else if modelResetAt != nil && creditsResetAt.Before(*modelResetAt) {
			// Once the credits cooldown expires, selection permits overages even
			// when the ordinary model quota is still cooling down.
			modelResetAt = creditsResetAt
		}
	}
	if modelResetAt != nil && (resetAt == nil || modelResetAt.After(*resetAt)) {
		resetAt = modelResetAt
	}
	if resetAt == nil {
		return nil
	}
	result := *resetAt
	return &result
}

// ModelAvailabilityDiagnoser is implemented by gateway services that can
// report whether the requested model is configured to be served by any
// account. Both *GatewayService and *OpenAIGatewayService implement this so
// handlers in either package can share a single classifier.
type ModelAvailabilityDiagnoser interface {
	DiagnoseModelAvailabilityForPlatform(
		ctx context.Context,
		groupID *int64,
		requestedModel string,
		platform string,
	) ModelAvailabilityDiagnosis
}

// DiagnoseModelAvailabilityForPlatform inspects accounts enabled for scheduling
// by persistent configuration and returns whether the requested model is
// configured to be served by any of them. The dedicated repository query
// bypasses scheduler snapshots and deliberately ignores transient rate-limit,
// overload, temporary-unschedulable, expiry, quota, and runtime-block state.
//
// Safe to call on the error path: returns {true,true} on any internal failure
// or when the inputs preclude meaningful diagnosis (empty model, etc.), so
// callers stay on the 503 fallback branch.
func (s *GatewayService) DiagnoseModelAvailabilityForPlatform(
	ctx context.Context,
	groupID *int64,
	requestedModel string,
	platform string,
) ModelAvailabilityDiagnosis {
	if s == nil {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		// No model specified — cannot decide model_not_found. Caller falls back to 503.
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}
	if strings.TrimSpace(platform) == "" {
		// Without a platform we cannot scope the lookup; bail out to the
		// 503 branch rather than make an unscoped scan.
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}

	if s.accountRepo == nil {
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}

	useMixed := platform == PlatformAnthropic || platform == PlatformGemini
	platforms := []string{platform}
	if useMixed {
		platforms = append(platforms, PlatformAntigravity)
	}

	queryGroupID := groupID
	includeGrouped := false
	if useMixed {
		// Preserve the generic scheduler's scope rules: an explicit group wins
		// for mixed scheduling, while group-less simple mode scans all accounts.
		if groupID == nil && s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
			includeGrouped = true
		}
	} else if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		queryGroupID = nil
		includeGrouped = true
	}

	accounts, err := s.accountRepo.ListModelAvailabilityCandidates(ctx, queryGroupID, platforms, includeGrouped)
	if err != nil {
		// Conservative fallback: pretend everything is fine so the caller
		// returns 503 (we don't want to flip to 404 just because a lookup
		// hiccup'd).
		return ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}
	}

	diag := ModelAvailabilityDiagnosis{Complete: true}
	now := time.Now()
	for i := range accounts {
		if useMixed && accounts[i].Platform == PlatformAntigravity && !accounts[i].IsMixedSchedulingEnabled() {
			continue
		}
		diag.recordAccount(ctx, &accounts[i], requestedModel, s.isModelSupportedByAccountInGroup(ctx, &accounts[i], groupID, requestedModel) &&
			!accounts[i].modelCapabilityRejectedForRequest(ctx, requestedModel), now)
	}
	return diag
}

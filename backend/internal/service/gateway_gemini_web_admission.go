package service

import (
	"context"
	"fmt"
)

// RevalidateGeminiWebAdmission refreshes the account after slot admission. A
// queued account may have been disabled, exhausted its quota, entered cooldown,
// or left the group while the request waited. Read the primary repository once
// here; alternative probes continue to use the scheduler's normal cache.
func (s *GatewayService) RevalidateGeminiWebAdmission(ctx context.Context, groupID *int64, selected *Account, requestedModel string) (*Account, error) {
	if selected == nil {
		return nil, ErrNoAvailableAccounts
	}
	if !selected.IsGeminiWeb() {
		return selected, nil
	}
	latest := selected
	var err error
	if s.accountRepo != nil {
		latest, err = s.accountRepo.GetByID(ctx, selected.ID)
	} else if s.schedulerSnapshot != nil {
		latest, err = s.schedulerSnapshot.GetAccount(ctx, selected.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("refresh Gemini Web admission: %w", err)
	}
	_, resolvedGroupID, err := s.checkClaudeCodeRestriction(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if latest == nil || !latest.IsGeminiWeb() ||
		!s.isAccountInGroup(latest, resolvedGroupID) ||
		!s.isAccountSchedulableForModelSelection(ctx, latest, requestedModel) ||
		(requestedModel != "" && !s.isModelSupportedByAccountInGroup(ctx, latest, resolvedGroupID, requestedModel)) ||
		s.isAccountBlockedBySchedulingThreshold(ctx, latest) {
		return nil, fmt.Errorf("%w: Gemini Web account is no longer eligible", ErrNoAvailableAccounts)
	}
	return latest, nil
}

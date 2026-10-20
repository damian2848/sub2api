package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

type geminiWebAdmissionRepo struct {
	AccountRepository
	account *Account
	err     error
	reads   int
}

func (r *geminiWebAdmissionRepo) GetByID(context.Context, int64) (*Account, error) {
	r.reads++
	return r.account, r.err
}

func TestRevalidateGeminiWebAdmission_UsesFreshEligibility(t *testing.T) {
	groupID := int64(7)
	future := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name   string
		change func(*Account)
	}{
		{"healthy", func(*Account) {}},
		{"cooldown", func(a *Account) { a.TempUnschedulableUntil = &future }},
		{"rate_limit", func(a *Account) { a.RateLimitResetAt = &future }},
		{"disabled", func(a *Account) { a.Schedulable = false }},
		{"removed_from_group", func(a *Account) { a.AccountGroups = nil }},
		{"model_removed", func(a *Account) { a.Credentials = map[string]any{"model_mapping": map[string]any{"other": "other"}} }},
		{"type_changed", func(a *Account) { a.Type = AccountTypeAPIKey }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeGeminiWeb, Status: StatusActive, Schedulable: true,
				AccountGroups: []AccountGroup{{GroupID: groupID}}, Credentials: map[string]any{"model_mapping": map[string]any{"gemini-4-argon": "gemini-4-argon"}}}
			latest := *selected
			tc.change(&latest)
			repo := &geminiWebAdmissionRepo{account: &latest}
			svc := &GatewayService{accountRepo: repo}
			ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformGemini)
			got, err := svc.RevalidateGeminiWebAdmission(ctx, &groupID, selected, "gemini-4-argon")
			require.Equal(t, 1, repo.reads)
			if tc.name == "healthy" {
				require.NoError(t, err)
				require.Same(t, &latest, got)
			} else {
				require.ErrorIs(t, err, ErrNoAvailableAccounts)
				require.Nil(t, got)
			}
			require.True(t, selected.Schedulable, "revalidation must not mutate the stale selection")
		})
	}
}

func TestRevalidateGeminiWebAdmission_ReadFailureDoesNotAdmitStaleAccount(t *testing.T) {
	failure := errors.New("repository unavailable")
	repo := &geminiWebAdmissionRepo{err: failure}
	svc := &GatewayService{accountRepo: repo}
	selected := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeGeminiWeb}
	got, err := svc.RevalidateGeminiWebAdmission(context.Background(), nil, selected, "gemini-4-argon")
	require.ErrorIs(t, err, failure)
	require.Nil(t, got)
	selected.Type = AccountTypeAPIKey
	got, err = svc.RevalidateGeminiWebAdmission(context.Background(), nil, selected, "gemini-4-argon")
	require.NoError(t, err)
	require.Same(t, selected, got)
	require.Equal(t, 1, repo.reads, "other providers must retain their admission policy")
}

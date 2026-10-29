//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminModelMappingUpdateRejectsInvalidPayloadBeforeReadingOrWriting(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		credentials map[string]any
	}{
		{"unknown mode", "append", map[string]any{"model_mapping": map[string]any{}}},
		{"missing credentials", "merge", nil},
		{"missing mapping", "replace", map[string]any{"other_setting": true}},
		{"null mapping", "merge", map[string]any{"model_mapping": nil}},
		{"array mapping", "merge", map[string]any{"model_mapping": []string{"model"}}},
		{"string mapping", "replace", map[string]any{"model_mapping": "model"}},
		{"non-string destination", "merge", map[string]any{"model_mapping": map[string]any{"model": 1}}},
		{"null destination", "replace", map[string]any{"model_mapping": map[string]any{"model": nil}}},
		{"blank source", "merge", map[string]any{"model_mapping": map[string]string{" ": "target"}}},
		{"blank destination", "replace", map[string]any{"model_mapping": map[string]string{"model": "\t"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &accountRepoStubForBulkUpdate{}
			svc := &adminServiceImpl{accountRepo: repo}
			result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
				AccountIDs: []int64{1}, ModelMappingUpdateMode: tt.mode, Credentials: tt.credentials,
			})
			require.Nil(t, result)
			requireApplicationErrorReason(t, err, "INVALID_MODEL_MAPPING_UPDATE_MODE")
			require.False(t, repo.getByIDsCalled)
			require.Zero(t, repo.bulkUpdateCalls)
		})
	}
}

func TestAdminModelMappingUpdateMergeRequiresEveryTargetToBeAntigravity(t *testing.T) {
	tests := []struct {
		name     string
		accounts []*Account
		loadErr  error
		wantErr  error
	}{
		{
			name: "mixed providers",
			accounts: []*Account{
				{ID: 1, Platform: PlatformAntigravity}, {ID: 2, Platform: PlatformGemini},
			},
		},
		{name: "missing target", accounts: []*Account{{ID: 1, Platform: PlatformAntigravity}}, wantErr: ErrAccountNotFound},
		{name: "nil target", accounts: []*Account{{ID: 1, Platform: PlatformAntigravity}, nil}, wantErr: ErrAccountNotFound},
		{name: "load failure", loadErr: errors.New("test lookup failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: tt.accounts, getByIDsErr: tt.loadErr}
			svc := &adminServiceImpl{accountRepo: repo}
			result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
				AccountIDs: []int64{1, 2}, ModelMappingUpdateMode: "merge",
				Credentials: map[string]any{"model_mapping": map[string]any{"new-model": "new-model"}},
			})
			require.Nil(t, result)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else if tt.loadErr != nil {
				require.ErrorIs(t, err, tt.loadErr)
			} else {
				requireApplicationErrorReason(t, err, "INVALID_MODEL_MAPPING_UPDATE_MODE")
			}
			require.True(t, repo.getByIDsCalled)
			require.Zero(t, repo.bulkUpdateCalls, "reject the whole request before any account is changed")
		})
	}
}

func TestAdminModelMappingUpdatePassesOnlyPatchWithAtomicMergeFlag(t *testing.T) {
	accounts := []*Account{
		{ID: 1, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{
			"access_token":  "fixture-account-one",
			"model_mapping": map[string]any{"old-alias": "gemini-4-argon-low"},
		}},
		{ID: 2, Platform: PlatformAntigravity, Type: AccountTypeAPIKey, Credentials: map[string]any{
			"api_key": "fixture-account-two",
		}},
		{ID: 3, Platform: PlatformAntigravity, Type: AccountTypeUpstream, Credentials: map[string]any{}},
	}
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: accounts}
	svc := &adminServiceImpl{accountRepo: repo}
	credentials := map[string]any{
		"model_mapping": map[string]string{
			"gemini-4-argon-low": "gemini-4-argon-low",
			"custom-flash":       "gemini-3.8-flash-high",
		},
		"intercept_warmup_requests": true,
	}
	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1, 2, 3}, ModelMappingUpdateMode: "merge", Credentials: credentials,
	})
	require.NoError(t, err)
	require.Equal(t, 3, result.Success)
	require.Equal(t, []int64{1, 2, 3}, repo.bulkUpdateIDs)
	require.Equal(t, 1, repo.bulkUpdateCalls)
	require.True(t, repo.lastBulkUpdate.MergeModelMapping)
	require.Equal(t, credentials, repo.lastBulkUpdate.Credentials,
		"the service must not materialize per-account mappings using a stale read")
	require.Empty(t, repo.getByIDCalled)
	require.Equal(t, map[string]any{"old-alias": "gemini-4-argon-low"}, accounts[0].Credentials["model_mapping"])
	require.NotContains(t, accounts[1].Credentials, "model_mapping")
}

func TestAdminModelMappingUpdateReplacementKeepsLegacyProviderSupport(t *testing.T) {
	for _, mode := range []string{"", "replace"} {
		t.Run("mode="+mode, func(t *testing.T) {
			repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{
				{ID: 1, Platform: PlatformAntigravity},
				{ID: 2, Platform: PlatformGemini},
			}}
			svc := &adminServiceImpl{accountRepo: repo}
			credentials := map[string]any{"model_mapping": map[string]any{}}
			result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
				AccountIDs: []int64{1, 2}, ModelMappingUpdateMode: mode, Credentials: credentials,
			})
			require.NoError(t, err)
			require.Equal(t, 2, result.Success)
			require.False(t, repo.lastBulkUpdate.MergeModelMapping)
			require.Equal(t, credentials, repo.lastBulkUpdate.Credentials)
		})
	}
}

func TestAdminModelMappingUpdateEmptyMergeDoesNotPopulateDefaultsInService(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{
		{ID: 1, Platform: PlatformAntigravity, Credentials: map[string]any{}},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1}, ModelMappingUpdateMode: "merge", Status: StatusActive,
		Credentials: map[string]any{"model_mapping": map[string]any{}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.True(t, repo.lastBulkUpdate.MergeModelMapping)
	require.Equal(t, map[string]any{"model_mapping": map[string]any{}}, repo.lastBulkUpdate.Credentials)
	require.NotNil(t, repo.lastBulkUpdate.Status)
	require.Equal(t, StatusActive, *repo.lastBulkUpdate.Status)
}

func TestAdminModelMappingUpdateFilteredSelectionUsesSameMergeContract(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{
		listData: []Account{
			{ID: 11, Platform: PlatformAntigravity}, {ID: 12, Platform: PlatformAntigravity},
		},
		getByIDsAccounts: []*Account{
			{ID: 11, Platform: PlatformAntigravity}, {ID: 12, Platform: PlatformAntigravity},
		},
	}
	svc := &adminServiceImpl{accountRepo: repo}
	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		Filters:                &BulkUpdateAccountFilters{Platform: PlatformAntigravity, Status: StatusActive},
		ModelMappingUpdateMode: "merge",
		Credentials:            map[string]any{"model_mapping": map[string]any{"custom": "gemini-4-argon-low"}},
	})
	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.True(t, repo.listCalled)
	require.Equal(t, PlatformAntigravity, repo.lastListFilters.platform)
	require.Equal(t, []int64{11, 12}, repo.bulkUpdateIDs)
	require.True(t, repo.lastBulkUpdate.MergeModelMapping)
}

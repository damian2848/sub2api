//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

func TestAccountFamilyGroupSurvivesOtherEditsAndCanBeCleared(t *testing.T) {
	repo := &longContextBillingRepoStub{account: &Account{
		ID: 1, Name: "one", Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Extra: map[string]any{AccountFamilyGroupKey: "家庭 A"},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Extra: map[string]any{"cost_multiplier": 0.2}})
	require.NoError(t, err)
	require.Equal(t, "家庭 A", updated.Extra[AccountFamilyGroupKey])
	updated, err = svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Extra: map[string]any{AccountFamilyGroupKey: ""}})
	require.NoError(t, err)
	require.Equal(t, "", updated.Extra[AccountFamilyGroupKey])
}

type familyFilterRepo struct {
	AccountRepository
	names []string
}

func (r *familyFilterRepo) ListWithFilters(ctx context.Context, p pagination.PaginationParams, platform, typ, status, search string, groupID int64, privacy string) ([]Account, *pagination.PaginationResult, error) {
	r.names = append(r.names, AccountFamilyGroupFilter(ctx))
	return []Account{{ID: int64(p.Page)}}, &pagination.PaginationResult{Total: 2}, nil
}
func TestAccountFamilyGroupBulkSelectionRetainsFilterAcrossPages(t *testing.T) {
	repo := &familyFilterRepo{}
	svc := &adminServiceImpl{accountRepo: repo}
	ids, err := svc.resolveBulkUpdateTargetIDs(context.Background(), &BulkUpdateAccountFilters{FamilyGroup: " 家庭 A "})
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, ids)
	require.Equal(t, []string{"家庭 A", "家庭 A"}, repo.names)
}

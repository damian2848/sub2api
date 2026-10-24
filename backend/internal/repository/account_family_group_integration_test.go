//go:build integration

package repository

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountFamilyGroupMembershipFilteringAndScope(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	group := mustCreateGroup(t, client, &service.Group{Name: "visible", Platform: service.PlatformAnthropic})
	var ids []int64
	for i, extra := range []map[string]any{
		{"family_group": "家庭 A", "other": "keep"}, {"family_group": "家庭 A"}, {"family_group": "家庭 B"},
		{}, {"family_group": ""}, {"family_group": nil}, {"family_group": "deleted"},
	} {
		a := mustCreateAccount(t, client, &service.Account{Name: "family", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, Extra: extra})
		ids = append(ids, a.ID)
		if i == 0 {
			require.NoError(t, repo.BindGroups(ctx, a.ID, []int64{group.ID}))
		}
	}
	require.NoError(t, repo.Delete(ctx, ids[6]))
	params := pagination.PaginationParams{Page: 1, PageSize: 1}
	filtered := service.WithAccountFamilyGroupFilter(ctx, "家庭 A")
	accounts, page, err := repo.ListWithFilters(filtered, params, "", "", "", "", 0, "")
	require.NoError(t, err)
	require.EqualValues(t, 2, page.Total)
	require.Len(t, accounts, 1)
	all, err := repo.ListAllWithFilters(filtered, "", "", "", "", 0, "")
	require.NoError(t, err)
	require.Len(t, all, 2)
	ungrouped := service.WithAccountFamilyGroupFilter(ctx, service.AccountFamilyGroupUnset)
	_, page, err = repo.ListWithFilters(ungrouped, params, "", "", "", "", 0, "")
	require.NoError(t, err)
	require.EqualValues(t, 3, page.Total)
	groups, err := repo.ListAccountFamilyGroups(ctx)
	require.NoError(t, err)
	require.Equal(t, []service.AccountFamilyGroup{{Name: "家庭 A", AccountCount: 2}, {Name: "家庭 B", AccountCount: 1}}, groups)
	scoped := service.WithObserverScope(ctx, []int64{group.ID})
	groups, err = repo.ListAccountFamilyGroups(scoped)
	require.NoError(t, err)
	require.Equal(t, []service.AccountFamilyGroup{{Name: "家庭 A", AccountCount: 1}}, groups)
	groups, err = repo.ListAccountFamilyGroups(service.WithObserverScope(ctx, []int64{}))
	require.NoError(t, err)
	require.Empty(t, groups)
	_, err = repo.BulkUpdate(ctx, []int64{ids[0], ids[1]}, service.AccountBulkUpdate{Extra: map[string]any{"family_group": ""}})
	require.NoError(t, err)
	groups, err = repo.ListAccountFamilyGroups(ctx)
	require.NoError(t, err)
	require.Equal(t, []service.AccountFamilyGroup{{Name: "家庭 B", AccountCount: 1}}, groups)
	account, err := repo.GetByID(ctx, ids[0])
	require.NoError(t, err)
	require.Equal(t, "keep", account.Extra["other"])
}

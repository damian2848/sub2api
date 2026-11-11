//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type countingOpenAIAccountRepo struct {
	schedulerTestOpenAIAccountRepo
	getByIDCalls *atomic.Int64
}

func (r countingOpenAIAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.getByIDCalls.Add(1)
	return r.schedulerTestOpenAIAccountRepo.GetByID(ctx, id)
}

func newSelectBestAccountRecheckTestAccounts(groupID int64, n int) []Account {
	accounts := make([]Account, 0, n)
	for i := 0; i < n; i++ {
		accounts = append(accounts, Account{
			ID:          int64(91000 + i),
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    i,
			GroupIDs:    []int64{groupID},
		})
	}
	return accounts
}

// newSelectBestAccountRecheckTestService serves snapshotAccounts from the
// scheduler snapshot and dbAccounts from the (counting) account repository.
func newSelectBestAccountRecheckTestService(snapshotAccounts, dbAccounts []Account) (*OpenAIGatewayService, *atomic.Int64) {
	snapshot := make([]*Account, 0, len(snapshotAccounts))
	byID := make(map[int64]*Account, len(snapshotAccounts))
	for i := range snapshotAccounts {
		acc := snapshotAccounts[i]
		snapshot = append(snapshot, &acc)
		byID[acc.ID] = &acc
	}
	calls := &atomic.Int64{}
	svc := &OpenAIGatewayService{
		accountRepo: countingOpenAIAccountRepo{
			schedulerTestOpenAIAccountRepo: schedulerTestOpenAIAccountRepo{accounts: dbAccounts},
			getByIDCalls:                   calls,
		},
		cfg: &config.Config{RunMode: config.RunModeStandard},
		schedulerSnapshot: &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{
			snapshotAccounts: snapshot,
			accountsByID:     byID,
		}},
	}
	return svc, calls
}

func TestSelectBestAccount_RechecksOnlyTheSelectedAccount(t *testing.T) {
	groupID := int64(93101)
	accounts := newSelectBestAccountRecheckTestAccounts(groupID, 50)
	svc, calls := newSelectBestAccountRecheckTestService(accounts, accounts)

	account, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, "", "", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(91000), account.ID, "priority 0 account must still win")
	require.Equal(t, int64(1), calls.Load(), "only the selected account is rechecked against the DB")
}

func TestSelectBestAccount_SkipsTopCandidateRejectedByDB(t *testing.T) {
	groupID := int64(93102)
	accounts := newSelectBestAccountRecheckTestAccounts(groupID, 5)
	dbAccounts := append([]Account(nil), accounts...)
	resetAt := time.Now().Add(time.Hour)
	dbAccounts[0].RateLimitResetAt = &resetAt // snapshot is stale, DB knows it is rate limited
	svc, calls := newSelectBestAccountRecheckTestService(accounts, dbAccounts)

	account, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, "", "", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(91001), account.ID)
	require.Equal(t, int64(2), calls.Load())
}

func TestSelectBestAccount_ReturnsDBCopyOrderedLikeSnapshot(t *testing.T) {
	groupID := int64(93103)
	accounts := newSelectBestAccountRecheckTestAccounts(groupID, 3)
	dbAccounts := append([]Account(nil), accounts...)
	dbAccounts[0].Name = "db-copy"

	svc, _ := newSelectBestAccountRecheckTestService(accounts, dbAccounts)
	selected, _, _ := svc.selectBestAccount(context.Background(), &groupID, PlatformOpenAI, append([]Account(nil), accounts...), "", nil, false, "", false)
	require.NotNil(t, selected)
	require.Equal(t, int64(91000), selected.ID)
	require.Equal(t, "db-copy", selected.Name, "the rechecked DB copy is returned")
}

func TestSelectBestAccount_AllRejectedKeepsFullFilterStats(t *testing.T) {
	groupID := int64(93104)
	accounts := newSelectBestAccountRecheckTestAccounts(groupID, 3)
	dbAccounts := append([]Account(nil), accounts...)
	for i := range dbAccounts {
		dbAccounts[i].Schedulable = false
	}
	svc, calls := newSelectBestAccountRecheckTestService(accounts, dbAccounts)

	_, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, "", "", map[int64]struct{}{91002: {}})
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Contains(t, err.Error(), "pool=3, filtered: excluded=1 ineligible=2")
	require.Equal(t, int64(2), calls.Load())
}

func TestSelectBestAccount_DBRecheckBudget(t *testing.T) {
	groupID := int64(93105)
	accounts := newSelectBestAccountRecheckTestAccounts(groupID, openAIAccountSelectionProbeLimit+10)
	dbAccounts := append([]Account(nil), accounts...)
	for i := range dbAccounts {
		dbAccounts[i].Schedulable = false
	}
	svc, calls := newSelectBestAccountRecheckTestService(accounts, dbAccounts)

	_, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, "", "", nil)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Equal(t, int64(openAIAccountSelectionProbeLimit), calls.Load())
}

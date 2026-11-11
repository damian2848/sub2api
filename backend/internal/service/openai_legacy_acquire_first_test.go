//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyCountingOpenAIAccountRepo counts DB rechecks (GetByID) on the legacy path.
type legacyCountingOpenAIAccountRepo struct {
	stubOpenAIAccountRepo
	getByID *atomic.Int64
}

func (r legacyCountingOpenAIAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.getByID.Add(1)
	return r.stubOpenAIAccountRepo.GetByID(ctx, id)
}

func legacyOpenAIAccount(id int64) Account {
	return Account{ID: id, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, GroupIDs: []int64{1}}
}

// legacyOpenAISnapshot serves the candidates from a scheduler snapshot, so the
// DB recheck (accountRepo.GetByID) is the only repository call on the path.
func legacyOpenAISnapshot(accounts []Account) *SchedulerSnapshotService {
	stub := &openAISnapshotCacheStub{accountsByID: map[int64]*Account{}}
	for i := range accounts {
		copied := accounts[i]
		stub.snapshotAccounts = append(stub.snapshotAccounts, &copied)
		stub.accountsByID[copied.ID] = &copied
	}
	return &SchedulerSnapshotService{cache: stub}
}

// Candidates whose slot cannot be taken no longer pay for a DB recheck: only
// the account that actually acquired a slot is rechecked.
func TestOpenAISelectAccountWithLoadAwareness_RechecksOnlyAcquiredCandidates(t *testing.T) {
	groupID := int64(1)
	calls := &atomic.Int64{}
	repo := legacyCountingOpenAIAccountRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{legacyOpenAIAccount(1), legacyOpenAIAccount(2), legacyOpenAIAccount(3)}},
		getByID:               calls,
	}
	concurrencyCache := stubConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			1: {AccountID: 1, LoadRate: 10},
			2: {AccountID: 2, LoadRate: 20},
			3: {AccountID: 3, LoadRate: 30},
		},
		acquireResults: map[int64]bool{1: false, 2: false},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              &stubGatewayCache{},
		schedulerSnapshot:  legacyOpenAISnapshot(repo.accounts),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-4", nil)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.True(t, selection.Acquired)
	require.Equal(t, int64(3), selection.Account.ID)
	require.Equal(t, int64(1), calls.Load(), "busy candidates must not be rechecked against the DB")
	require.Same(t, &repo.accounts[2], selection.Account, "the rechecked DB copy is returned, not a re-hydrated snapshot copy")
}

// With load_factor above concurrency an account can sit below 100% load while
// every hard slot is taken. The load snapshot already shows that, so the
// legacy path skips it instead of trying to acquire.
func TestOpenAISelectAccountWithLoadAwareness_SkipsHardFullCandidates(t *testing.T) {
	groupID := int64(1)
	calls := &atomic.Int64{}
	repo := legacyCountingOpenAIAccountRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{legacyOpenAIAccount(1), legacyOpenAIAccount(2)}},
		getByID:               calls,
	}
	concurrencyCache := stubConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			1: {AccountID: 1, LoadRate: 50, CurrentConcurrency: 1},
			2: {AccountID: 2, LoadRate: 60},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              &stubGatewayCache{},
		schedulerSnapshot:  legacyOpenAISnapshot(repo.accounts),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-4", nil)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.True(t, selection.Acquired)
	require.Equal(t, int64(2), selection.Account.ID, "the lower-load account has no free hard slot")
	require.Equal(t, int64(1), calls.Load())
}

//go:build unit

package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	fallbackSlotsGroupID = int64(23)
	fallbackSlotsModel   = "gemini-4-carbon-high"
	fallbackBigAccount   = int64(357)
	fallbackSlowAccount  = int64(326)
	fallbackSmallAccount = int64(314)
)

// fallbackSlotCache keeps real slot and queue accounting, so these tests run
// load_factor against the hard concurrency limit instead of canned load rates.
type fallbackSlotCache struct {
	mockConcurrencyCache
	mu              sync.Mutex
	inFlight        map[int64]int
	waiting         map[int64]int
	rejectAcquire   map[int64]bool
	acquireAttempts []int64
}

func (c *fallbackSlotCache) AcquireAccountSlot(_ context.Context, accountID int64, maxConcurrency int, _ string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acquireAttempts = append(c.acquireAttempts, accountID)
	if c.rejectAcquire[accountID] || c.inFlight[accountID] >= maxConcurrency {
		return false, nil
	}
	c.inFlight[accountID]++
	return true, nil
}

func (c *fallbackSlotCache) ReleaseAccountSlot(_ context.Context, accountID int64, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight[accountID]--
	return nil
}

func (c *fallbackSlotCache) GetAccountsLoadBatch(_ context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[int64]*AccountLoadInfo, len(accounts))
	for _, acc := range accounts {
		current, waiting := c.inFlight[acc.ID], c.waiting[acc.ID]
		result[acc.ID] = &AccountLoadInfo{
			AccountID:          acc.ID,
			CurrentConcurrency: current,
			WaitingCount:       waiting,
			LoadRate:           (current + waiting) * 100 / acc.MaxConcurrency,
		}
	}
	return result, nil
}

// lockedGroupRepo keeps the shared mock's plain call counters safe under the
// concurrent burst test.
type lockedGroupRepo struct {
	mockGroupRepoForGateway
	mu sync.Mutex
}

func (r *lockedGroupRepo) GetByIDLite(ctx context.Context, id int64) (*Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mockGroupRepoForGateway.GetByIDLite(ctx, id)
}

func (c *fallbackSlotCache) load(accountID int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight[accountID]
}

func fallbackGeminiAccount(id int64, priority, concurrency, loadFactor int, lastUsed time.Time) Account {
	return Account{
		ID:          id,
		Platform:    PlatformGemini,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Priority:    priority,
		Concurrency: concurrency,
		LoadFactor:  intPtrHelper(loadFactor),
		LastUsedAt:  &lastUsed,
	}
}

func newFallbackSlotsService(cache *fallbackSlotCache, gatewayCache *mockGatewayCacheForPlatform, accounts ...Account) *GatewayService {
	repo := &mockAccountRepoForPlatform{accounts: accounts, accountsByID: map[int64]*Account{}}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	groupRepo := &lockedGroupRepo{mockGroupRepoForGateway: mockGroupRepoForGateway{groups: map[int64]*Group{
		fallbackSlotsGroupID: {ID: fallbackSlotsGroupID, Platform: PlatformGemini, Status: StatusActive, Hydrated: true},
	}}}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Gateway.Scheduling.FallbackMaxWaiting = 100
	cfg.Gateway.Scheduling.FallbackWaitTimeout = 30 * time.Second
	cfg.Gateway.Scheduling.FallbackProbeHardSlots = true
	return &GatewayService{
		accountRepo:        repo,
		groupRepo:          groupRepo,
		cache:              gatewayCache,
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(cache),
	}
}

func selectFallbackSlotsAccount(t *testing.T, svc *GatewayService, ctx context.Context, sessionHash string) *AccountSelectionResult {
	t.Helper()
	groupID := fallbackSlotsGroupID
	result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, sessionHash, fallbackSlotsModel, nil, "", int64(0))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Account)
	return result
}

// Production sample from 2026-11-02 17:40: the least recently used account was
// out of slots with a nearly full queue while the big account had 99 free slots.
func TestSelectAccountFallbackUsesFreeHardSlotBeforeQueueing(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{
		inFlight: map[int64]int{fallbackBigAccount: 21, fallbackSlowAccount: 30, fallbackSmallAccount: 30},
		waiting:  map[int64]int{fallbackSlowAccount: 99, fallbackSmallAccount: 40},
	}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
		fallbackGeminiAccount(fallbackSlowAccount, 1, 30, 1, now.Add(-time.Minute)),
		fallbackGeminiAccount(fallbackSmallAccount, 1, 30, 1, now.Add(-2*time.Minute)),
	)

	result := selectFallbackSlotsAccount(t, svc, context.Background(), "")

	require.True(t, result.Acquired, "free hard slots must be used before any queue")
	require.Nil(t, result.WaitPlan)
	require.Equal(t, fallbackBigAccount, result.Account.ID)
	require.Equal(t, 22, cache.load(fallbackBigAccount))
	result.ReleaseFunc()
	require.Equal(t, 21, cache.load(fallbackBigAccount))
}

// Production sample from 2026-11-02 17:51: the priority 1 account was full with
// a full queue, and the request was rejected while priority 2 had free slots.
func TestSelectAccountFallbackMovesToNextPriorityWhenPrimaryIsHardFull(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{
		inFlight: map[int64]int{fallbackBigAccount: 120, fallbackSlowAccount: 10, fallbackSmallAccount: 1},
		waiting:  map[int64]int{fallbackBigAccount: 100},
	}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now.Add(-time.Hour)),
		fallbackGeminiAccount(fallbackSlowAccount, 2, 10, 1, now),
		fallbackGeminiAccount(fallbackSmallAccount, 2, 10, 1, now),
	)

	result := selectFallbackSlotsAccount(t, svc, context.Background(), "")

	require.True(t, result.Acquired)
	require.Nil(t, result.WaitPlan)
	require.Equal(t, fallbackSmallAccount, result.Account.ID)
}

func TestSelectAccountFallbackKeepsPriorityOrderForHardSlots(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{
		inFlight: map[int64]int{fallbackBigAccount: 15, fallbackSlowAccount: 1},
	}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
		fallbackGeminiAccount(fallbackSlowAccount, 2, 10, 1, now.Add(-time.Hour)),
	)

	result := selectFallbackSlotsAccount(t, svc, context.Background(), "")

	require.True(t, result.Acquired)
	require.Equal(t, fallbackBigAccount, result.Account.ID, "a smaller priority keeps winning past its soft capacity")
}

// With every hard slot taken the request stays on the first candidate, even when
// its queue is full: the handler rejects at once instead of parking the request
// on a slower account's queue until it times out.
func TestSelectAccountFallbackKeepsFirstCandidateWhenEveryHardSlotIsTaken(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{
		inFlight: map[int64]int{fallbackBigAccount: 120, fallbackSlowAccount: 10},
		waiting:  map[int64]int{fallbackBigAccount: 100, fallbackSlowAccount: 7},
	}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
		fallbackGeminiAccount(fallbackSlowAccount, 2, 10, 1, now),
	)

	result := selectFallbackSlotsAccount(t, svc, context.Background(), "")

	require.False(t, result.Acquired)
	require.NotNil(t, result.WaitPlan, "the handler turns a full queue into the normal 429")
	require.Equal(t, fallbackBigAccount, result.Account.ID)
	require.Equal(t, 120, result.WaitPlan.MaxConcurrency)
	require.Equal(t, 100, result.WaitPlan.MaxWaiting)
	require.Equal(t, 30*time.Second, result.WaitPlan.Timeout)
	require.Empty(t, cache.acquireAttempts, "accounts known to be hard full must not cost a Redis acquire")
}

func TestSelectAccountFallbackContinuesWhenSlotRaceIsLost(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{
		inFlight:      map[int64]int{fallbackBigAccount: 15, fallbackSlowAccount: 1},
		rejectAcquire: map[int64]bool{fallbackBigAccount: true},
	}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
		fallbackGeminiAccount(fallbackSlowAccount, 2, 10, 1, now),
	)

	result := selectFallbackSlotsAccount(t, svc, context.Background(), "")

	require.True(t, result.Acquired)
	require.Equal(t, fallbackSlowAccount, result.Account.ID)
}

func TestSelectAccountFallbackBindsStickyOnlyWhenNotDeferred(t *testing.T) {
	now := time.Now()
	newService := func(gatewayCache *mockGatewayCacheForPlatform) *GatewayService {
		cache := &fallbackSlotCache{inFlight: map[int64]int{fallbackBigAccount: 15, fallbackSlowAccount: 10}}
		return newFallbackSlotsService(cache, gatewayCache,
			fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
			fallbackGeminiAccount(fallbackSlowAccount, 2, 10, 1, now),
		)
	}

	bound := &mockGatewayCacheForPlatform{}
	result := selectFallbackSlotsAccount(t, newService(bound), context.Background(), "gemini:bound")
	require.True(t, result.Acquired)
	require.Equal(t, fallbackBigAccount, bound.sessionBindings["gemini:bound"])

	deferred := &mockGatewayCacheForPlatform{}
	result = selectFallbackSlotsAccount(t, newService(deferred), WithDeferredGatewayStickyBinding(context.Background()), "gemini:deferred")
	require.True(t, result.Acquired)
	require.NotContains(t, deferred.sessionBindings, "gemini:deferred")
}

// Replays the 17:51 burst shape: nothing finishes while it arrives, so every
// hard slot in the pool fills exactly once before the first request queues.
func TestSelectAccountFallbackBurstFillsEveryHardSlotBeforeQueueing(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{inFlight: map[int64]int{}, waiting: map[int64]int{}}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
		fallbackGeminiAccount(fallbackSlowAccount, 2, 10, 1, now),
		fallbackGeminiAccount(fallbackSmallAccount, 2, 10, 1, now),
	)

	const requests = 400
	acquired, queued := 0, 0
	for range requests {
		result := selectFallbackSlotsAccount(t, svc, context.Background(), "")
		if result.Acquired {
			require.Zero(t, queued, "a request queued while a hard slot was still free")
			acquired++
			continue
		}
		require.NotNil(t, result.WaitPlan)
		queued++
	}

	require.Equal(t, 140, acquired)
	require.Equal(t, requests-140, queued)
	require.Equal(t, 120, cache.load(fallbackBigAccount))
	require.Equal(t, 10, cache.load(fallbackSlowAccount))
	require.Equal(t, 10, cache.load(fallbackSmallAccount))
}

// Concurrent callers read the same load snapshot, so this guards the hard limits
// rather than the selection order that the sequential replay above pins down.
func TestSelectAccountFallbackConcurrentBurstNeverExceedsHardSlots(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{inFlight: map[int64]int{}, waiting: map[int64]int{}}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
		fallbackGeminiAccount(fallbackSlowAccount, 2, 10, 1, now),
		fallbackGeminiAccount(fallbackSmallAccount, 2, 10, 1, now),
	)

	const requests = 400
	var acquired, queued atomic.Int64
	var wg sync.WaitGroup
	groupID := fallbackSlotsGroupID
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", fallbackSlotsModel, nil, "", int64(0))
			if err != nil || result == nil {
				return
			}
			if result.Acquired {
				acquired.Add(1)
			} else if result.WaitPlan != nil {
				queued.Add(1)
			}
		}()
	}
	wg.Wait()

	require.Equal(t, int64(140), acquired.Load())
	require.Equal(t, int64(requests-140), queued.Load())
	require.Equal(t, 120, cache.load(fallbackBigAccount))
	require.Equal(t, 10, cache.load(fallbackSlowAccount))
	require.Equal(t, 10, cache.load(fallbackSmallAccount))
}

func TestSelectAccountFallbackAllowsUnlimitedConcurrencyAccounts(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{inFlight: map[int64]int{fallbackBigAccount: 5}}
	account := fallbackGeminiAccount(fallbackBigAccount, 1, 0, 1, now)
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{}, account)

	result := selectFallbackSlotsAccount(t, svc, context.Background(), "")

	require.True(t, result.Acquired, "concurrency 0 means no hard limit")
	require.Nil(t, result.WaitPlan)
}

// fallback_probe_hard_slots=false restores the old Layer 3: queue on the first
// candidate without probing any slot.
func TestSelectAccountFallbackProbeDisabledKeepsQueueing(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{
		inFlight: map[int64]int{fallbackBigAccount: 21, fallbackSlowAccount: 30},
		waiting:  map[int64]int{fallbackSlowAccount: 99},
	}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
		fallbackGeminiAccount(fallbackSlowAccount, 1, 30, 1, now.Add(-time.Minute)),
	)
	svc.cfg.Gateway.Scheduling.FallbackProbeHardSlots = false

	result := selectFallbackSlotsAccount(t, svc, context.Background(), "")

	require.False(t, result.Acquired)
	require.NotNil(t, result.WaitPlan)
	require.Equal(t, fallbackSlowAccount, result.Account.ID, "old behaviour queues on the least recently used candidate")
	require.Empty(t, cache.acquireAttempts)
}

// An account whose acquire already failed in Layer 2 is not acquired a second
// time in the Layer 3 probe within the same selection.
func TestSelectAccountFallbackSkipsAccountsTriedInLayer2(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{
		inFlight:      map[int64]int{fallbackBigAccount: 0, fallbackSlowAccount: 1},
		rejectAcquire: map[int64]bool{fallbackBigAccount: true},
	}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
		fallbackGeminiAccount(fallbackSlowAccount, 2, 10, 1, now),
	)

	result := selectFallbackSlotsAccount(t, svc, context.Background(), "")

	require.True(t, result.Acquired)
	require.Equal(t, fallbackSlowAccount, result.Account.ID)
	require.Equal(t, []int64{fallbackBigAccount, fallbackSlowAccount}, cache.acquireAttempts)
}

// Requests already queued on an account retry with backoff; a new arrival that
// grabbed a slot freed there would jump ahead of them. The probe leaves that
// slot to the queue and only uses slots not already owed to waiters.
func TestSelectAccountFallbackDoesNotJumpAheadOfQueuedRequests(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{
		inFlight: map[int64]int{fallbackSlowAccount: 29, fallbackBigAccount: 120, fallbackSmallAccount: 5},
		waiting:  map[int64]int{fallbackSlowAccount: 100},
	}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackSlowAccount, 1, 30, 1, now.Add(-time.Hour)),
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
		fallbackGeminiAccount(fallbackSmallAccount, 2, 10, 1, now),
	)

	result := selectFallbackSlotsAccount(t, svc, context.Background(), "")

	require.True(t, result.Acquired, "slots on accounts without waiters are still probed")
	require.Equal(t, fallbackSmallAccount, result.Account.ID)
	require.Equal(t, 29, cache.load(fallbackSlowAccount), "the slot freed on the queued account stays for its waiters")
	require.NotContains(t, cache.acquireAttempts, fallbackSlowAccount)
}

// When the only free slots are owed to queued requests, the new request queues
// behind them on the first candidate instead of acquiring.
func TestSelectAccountFallbackQueuesBehindWaitersWhenFreeSlotsAreOwed(t *testing.T) {
	now := time.Now()
	cache := &fallbackSlotCache{
		inFlight: map[int64]int{fallbackSlowAccount: 29, fallbackBigAccount: 118},
		waiting:  map[int64]int{fallbackSlowAccount: 1, fallbackBigAccount: 2},
	}
	svc := newFallbackSlotsService(cache, &mockGatewayCacheForPlatform{},
		fallbackGeminiAccount(fallbackSlowAccount, 1, 30, 1, now.Add(-time.Hour)),
		fallbackGeminiAccount(fallbackBigAccount, 1, 120, 15, now),
	)

	result := selectFallbackSlotsAccount(t, svc, context.Background(), "")

	require.False(t, result.Acquired)
	require.NotNil(t, result.WaitPlan)
	require.Equal(t, fallbackSlowAccount, result.Account.ID)
	require.Empty(t, cache.acquireAttempts)
}

func TestFallbackAccountHasFreeSlotCountsWaiters(t *testing.T) {
	acc := &Account{Concurrency: 30}
	require.False(t, fallbackAccountHasFreeSlot(acc, nil))
	require.True(t, fallbackAccountHasFreeSlot(acc, &AccountLoadInfo{CurrentConcurrency: 29}))
	require.False(t, fallbackAccountHasFreeSlot(acc, &AccountLoadInfo{CurrentConcurrency: 29, WaitingCount: 1}))
	require.True(t, fallbackAccountHasFreeSlot(acc, &AccountLoadInfo{CurrentConcurrency: 27, WaitingCount: 2}))
	require.False(t, fallbackAccountHasFreeSlot(acc, &AccountLoadInfo{CurrentConcurrency: 30}))
	require.True(t, fallbackAccountHasFreeSlot(&Account{Concurrency: 0}, &AccountLoadInfo{CurrentConcurrency: 5, WaitingCount: 9}))
}

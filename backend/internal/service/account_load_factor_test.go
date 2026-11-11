//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func intPtrHelper(v int) *int { return &v }

func TestEffectiveLoadFactor_NilAccount(t *testing.T) {
	var a *Account
	require.Equal(t, 1, a.EffectiveLoadFactor())
}

func TestEffectiveLoadFactor_NilLoadFactor_PositiveConcurrency(t *testing.T) {
	a := &Account{Concurrency: 5}
	require.Equal(t, 5, a.EffectiveLoadFactor())
}

func TestEffectiveLoadFactor_NilLoadFactor_ZeroConcurrency(t *testing.T) {
	a := &Account{Concurrency: 0}
	require.Equal(t, 1, a.EffectiveLoadFactor())
}

func TestEffectiveLoadFactor_PositiveLoadFactor(t *testing.T) {
	a := &Account{Concurrency: 5, LoadFactor: intPtrHelper(20)}
	require.Equal(t, 20, a.EffectiveLoadFactor())
}

func TestEffectiveLoadFactor_ZeroLoadFactor_FallbackToConcurrency(t *testing.T) {
	a := &Account{Concurrency: 5, LoadFactor: intPtrHelper(0)}
	require.Equal(t, 5, a.EffectiveLoadFactor())
}

func TestEffectiveLoadFactor_NegativeLoadFactor_FallbackToConcurrency(t *testing.T) {
	a := &Account{Concurrency: 3, LoadFactor: intPtrHelper(-1)}
	require.Equal(t, 3, a.EffectiveLoadFactor())
}

func TestEffectiveLoadFactor_ZeroLoadFactor_ZeroConcurrency(t *testing.T) {
	a := &Account{Concurrency: 0, LoadFactor: intPtrHelper(0)}
	require.Equal(t, 1, a.EffectiveLoadFactor())
}

func TestBulkUpdateAccountsLoadFactor(t *testing.T) {
	bulkUpdate := func(t *testing.T, loadFactor *int) (*upstreamBillingProbeAccountRepo, error) {
		t.Helper()
		repo := &upstreamBillingProbeAccountRepo{}
		_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
			AccountIDs: []int64{301},
			LoadFactor: loadFactor,
		})
		return repo, err
	}

	for _, value := range []int{0, -1} {
		repo, err := bulkUpdate(t, intPtrHelper(value))
		require.NoError(t, err)
		require.Len(t, repo.bulkUpdates, 1)
		require.NotNil(t, repo.bulkUpdates[0].LoadFactor, "clearing must reach the repository, nil means keep")
		require.Equal(t, value, *repo.bulkUpdates[0].LoadFactor)
	}

	repo, err := bulkUpdate(t, intPtrHelper(15))
	require.NoError(t, err)
	require.Equal(t, 15, *repo.bulkUpdates[0].LoadFactor)

	repo, err = bulkUpdate(t, nil)
	require.NoError(t, err)
	require.Nil(t, repo.bulkUpdates[0].LoadFactor)

	repo, err = bulkUpdate(t, intPtrHelper(10001))
	require.ErrorContains(t, err, "load_factor must be <= 10000")
	require.Empty(t, repo.bulkUpdates)
}

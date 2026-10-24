package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountFamilyGroupValidation(t *testing.T) {
	require.NoError(t, NormalizeAccountFamilyGroupExtra(nil))
	for _, tc := range []struct {
		raw     any
		want    string
		invalid bool
	}{
		{" 家庭 A ", "家庭 A", false}, {nil, "", false}, {"   ", "", false},
		{strings.Repeat("家", 100), strings.Repeat("家", 100), false},
		{strings.Repeat("家", 101), "", true}, {"a\nb", "", true},
		{AccountFamilyGroupUnset, "", true}, {123, "", true},
	} {
		extra := map[string]any{AccountFamilyGroupKey: tc.raw, "cost_multiplier": 0.2}
		err := NormalizeAccountFamilyGroupExtra(extra)
		if tc.invalid {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.want, extra[AccountFamilyGroupKey])
		}
		require.Equal(t, 0.2, extra["cost_multiplier"])
	}
}

func TestAccountFamilyGroupInvalidWritesRejectedBeforeRepository(t *testing.T) {
	s := &adminServiceImpl{}
	ctx := context.Background()
	extra := map[string]any{AccountFamilyGroupKey: []string{"A", "B"}}
	_, err := s.CreateAccount(ctx, &CreateAccountInput{Extra: extra})
	require.ErrorContains(t, err, "family group")
	_, err = s.UpdateAccount(ctx, 1, &UpdateAccountInput{Extra: extra})
	require.ErrorContains(t, err, "family group")
	_, err = s.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Extra: extra})
	require.ErrorContains(t, err, "family group")
	require.ErrorContains(t, s.UpdateAccountExtra(ctx, 1, extra), "family group")
}

//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func antigravityAvailabilityTestAccount(id int64, models ...string) Account {
	mapping := make(map[string]any, len(models))
	for _, model := range models {
		mapping[model] = model
	}
	return Account{
		ID: id, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
		AccountGroups: []AccountGroup{{GroupID: 52}},
		Credentials:   map[string]any{"model_mapping": mapping},
	}
}

func diagnoseAntigravityTestAccounts(ctx context.Context, accounts []Account, model string) ModelAvailabilityDiagnosis {
	repo := &mockAccountRepoForPlatform{accounts: accounts, accountsByID: map[int64]*Account{}}
	svc := &GatewayService{accountRepo: repo, cfg: testConfig()}
	groupID := int64(52)
	return svc.DiagnoseModelAvailabilityForPlatform(ctx, &groupID, model, PlatformAntigravity)
}

func TestAntigravityModelCapacityDiagnosis_ScopeOnlyCooldowns(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	early, late := now.Add(time.Minute), now.Add(72*time.Hour)
	accounts := []Account{
		antigravityAvailabilityTestAccount(280, "gemini-4-argon-high"),
		antigravityAvailabilityTestAccount(288, "gemini-4-argon-high"),
		antigravityAvailabilityTestAccount(286, "gemini-3.8-flash-high"),
	}
	setAccountModelRateLimitSnapshot(&accounts[0], antigravityGeminiModelRateLimitKey, late, "", now)
	setAccountModelRateLimitSnapshot(&accounts[1], antigravityGeminiModelRateLimitKey, early, "", now)

	diag := diagnoseAntigravityTestAccounts(context.Background(), accounts, "gemini-4-argon-high")

	require.True(t, diag.Complete)
	require.True(t, diag.HasModelSupport, "cooldowns must not turn a configured model into a 404")
	require.Equal(t, 3, diag.ConfiguredAccounts)
	require.Equal(t, 2, diag.ModelSupportedAccounts)
	require.Equal(t, 2, diag.ModelRateLimitedAccounts)
	require.True(t, diag.AllModelAccountsRateLimited())
	require.Equal(t, early, *diag.EarliestRateLimitResetAt)
}

func TestAntigravityModelCapacityDiagnosis_LatestCooldownPerAccount(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	accounts := []Account{
		antigravityAvailabilityTestAccount(288, "gemini-4-argon-high"),
		antigravityAvailabilityTestAccount(296, "gemini-4-argon-high"),
	}
	firstGlobal, secondGlobal := now.Add(time.Hour), now.Add(90*time.Minute)
	accounts[0].RateLimitResetAt, accounts[1].RateLimitResetAt = &firstGlobal, &secondGlobal
	setAccountModelRateLimitSnapshot(&accounts[0], antigravityGeminiModelRateLimitKey, now.Add(30*time.Minute), "", now)
	setAccountModelRateLimitSnapshot(&accounts[0], "gemini-4-argon-high", now.Add(2*time.Hour), "", now)
	setAccountModelRateLimitSnapshot(&accounts[1], antigravityGeminiModelRateLimitKey, now.Add(time.Hour), "", now)
	setAccountModelRateLimitSnapshot(&accounts[1], "gemini-4-argon-high", now.Add(30*time.Minute), "", now)
	setAccountModelRateLimitSnapshot(&accounts[1], "claude-sonnet-4-5", now.Add(5*time.Hour), "", now)

	diag := diagnoseAntigravityTestAccounts(context.Background(), accounts, "gemini-4-argon-high")

	require.True(t, diag.AllModelAccountsRateLimited())
	require.Equal(t, secondGlobal, *diag.EarliestRateLimitResetAt,
		"first account needs 2h, second needs 90m; unrelated model cooldowns must not extend either")
}

func TestAntigravityModelCapacityDiagnosis_DoesNotCountUnrelatedOrExpiredScopes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	future, past := now.Add(time.Hour), now.Add(-time.Minute)
	accounts := []Account{
		antigravityAvailabilityTestAccount(288, "gemini-4-argon-high", "claude-sonnet-4-5"),
		antigravityAvailabilityTestAccount(296, "gemini-4-argon-high", "claude-sonnet-4-5"),
	}
	setAccountModelRateLimitSnapshot(&accounts[0], antigravityGeminiModelRateLimitKey, future, "", now)
	setAccountModelRateLimitSnapshot(&accounts[1], "claude-sonnet-4-5", future, "", now)
	accounts[1].RateLimitResetAt = &past

	for _, model := range []string{"gemini-4-argon-high", "claude-sonnet-4-5"} {
		diag := diagnoseAntigravityTestAccounts(context.Background(), accounts, model)
		require.Equal(t, 2, diag.ModelSupportedAccounts, model)
		require.Equal(t, 1, diag.ModelRateLimitedAccounts, model)
		require.False(t, diag.AllModelAccountsRateLimited(), model)
	}

	setAccountModelRateLimitSnapshot(&accounts[0], antigravityGeminiModelRateLimitKey, past, "", now)
	diag := diagnoseAntigravityTestAccounts(context.Background(), accounts, "gemini-4-argon-high")
	require.Zero(t, diag.ModelRateLimitedAccounts)
	require.Nil(t, diag.EarliestRateLimitResetAt)
}

func TestAntigravityModelCapacityDiagnosis_Overages(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	modelReset, earlyCreditReset, lateCreditReset := now.Add(time.Hour), now.Add(30*time.Minute), now.Add(2*time.Hour)
	expired, globalReset := now.Add(-time.Minute), now.Add(90*time.Minute)
	for _, tc := range []struct {
		name         string
		overages     bool
		creditsReset *time.Time
		globalReset  *time.Time
		wantReset    *time.Time
	}{
		{name: "ordinary quota", wantReset: &modelReset},
		{name: "credits available", overages: true},
		{name: "credits cooldown expired", overages: true, creditsReset: &expired},
		{name: "credits recover first", overages: true, creditsReset: &earlyCreditReset, wantReset: &earlyCreditReset},
		{name: "model quota recovers first", overages: true, creditsReset: &lateCreditReset, wantReset: &modelReset},
		{name: "global cooldown cannot be bypassed", overages: true, globalReset: &globalReset, wantReset: &globalReset},
		{name: "global outlasts credits", overages: true, creditsReset: &earlyCreditReset, globalReset: &globalReset, wantReset: &globalReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := antigravityAvailabilityTestAccount(288, "gemini-4-argon-high")
			setAccountModelRateLimitSnapshot(&account, antigravityGeminiModelRateLimitKey, modelReset, "", now)
			account.Extra["allow_overages"] = tc.overages
			account.RateLimitResetAt = tc.globalReset
			if tc.creditsReset != nil {
				setAccountModelRateLimitSnapshot(&account, creditsExhaustedKey, *tc.creditsReset, "", now)
			}

			diag := diagnoseAntigravityTestAccounts(context.Background(), []Account{account}, "gemini-4-argon-high")

			require.True(t, diag.HasModelSupport)
			if tc.wantReset == nil {
				require.True(t, account.IsSchedulableForModelWithContext(context.Background(), "gemini-4-argon-high"))
				require.Zero(t, diag.ModelRateLimitedAccounts)
				require.Nil(t, diag.EarliestRateLimitResetAt)
				require.False(t, diag.AllModelAccountsRateLimited())
			} else {
				require.False(t, account.IsSchedulableForModelWithContext(context.Background(), "gemini-4-argon-high"))
				require.True(t, diag.AllModelAccountsRateLimited())
				require.Equal(t, *tc.wantReset, *diag.EarliestRateLimitResetAt)
			}
		})
	}
}

func TestAntigravityModelCapacityDiagnosis_ResolvesMappedModelAndThinkingContext(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	resetAt := now.Add(time.Hour)
	account := antigravityAvailabilityTestAccount(288, "claude-sonnet-4-5", "claude-sonnet-4-5-thinking")
	account.Credentials["model_mapping"].(map[string]any)["public-sonnet"] = "claude-sonnet-4-5"
	setAccountModelRateLimitSnapshot(&account, "claude-sonnet-4-5-thinking", resetAt, "", now)

	diag := diagnoseAntigravityTestAccounts(context.Background(), []Account{account}, "public-sonnet")
	require.True(t, diag.HasModelSupport)
	require.Zero(t, diag.ModelRateLimitedAccounts)

	ctx := WithThinkingEnabled(context.Background(), true, false)
	diag = diagnoseAntigravityTestAccounts(ctx, []Account{account}, "public-sonnet")
	require.True(t, diag.AllModelAccountsRateLimited())
	require.Equal(t, resetAt, *diag.EarliestRateLimitResetAt)
}

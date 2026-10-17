//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGeminiWebResponseModelUsesStandardBillingPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		source   string
		conflict bool
		want     string
	}{
		{"default preserves requested billing", "", false, "gemini-4-argon"},
		{"response source adopts cheaper model", BillingModelSourceResponse, false, "gemini-3.5-flash-lite"},
		{"conflicting declarations use baseline", BillingModelSourceResponse, true, "gemini-4-argon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			userRepo := &openAIRecordUsageUserRepoStub{}
			svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{})
			tokens := UsageTokens{InputTokens: 100, OutputTokens: 50}
			wantCost, err := svc.billingService.CalculateCost(tc.want, tokens, 1.1)
			require.NoError(t, err)
			require.True(t, svc.billingService.HasIdentifiedTokenPricing("gemini-3.5-flash-lite"))
			err = svc.RecordUsage(context.Background(), &RecordUsageInput{
				Result: &ForwardResult{
					RequestID: "gemini_web_model_audit", Model: "gemini-4-argon", UpstreamModel: "gemini-4-argon",
					UpstreamResponseModel: "gemini-3.5-flash-lite", UpstreamResponseModelConflict: tc.conflict,
					Usage: ClaudeUsage{InputTokens: 100, OutputTokens: 50}, Duration: time.Second,
				},
				APIKey: &APIKey{ID: 501, Quota: 100}, User: &User{ID: 601},
				Account: &Account{ID: 701, Platform: PlatformGemini, Type: AccountTypeGeminiWeb},
				ChannelUsageFields: ChannelUsageFields{
					ChannelID: 9, OriginalModel: "gemini-4-argon", ChannelMappedModel: "gemini-4-argon", BillingModelSource: tc.source,
				},
			})
			require.NoError(t, err)
			require.NotNil(t, usageRepo.lastLog)
			require.InDelta(t, wantCost.ActualCost, usageRepo.lastLog.ActualCost, 1e-12)
			require.InDelta(t, wantCost.ActualCost, userRepo.lastAmount, 1e-12)
			require.Equal(t, "gemini-4-argon", usageRepo.lastLog.RequestedModel)
			require.NotNil(t, usageRepo.lastLog.UpstreamModel)
			require.Equal(t, "gemini-4-argon", *usageRepo.lastLog.UpstreamModel)
			require.NotNil(t, usageRepo.lastLog.UpstreamResponseModel)
			require.Equal(t, "gemini-3.5-flash-lite", *usageRepo.lastLog.UpstreamResponseModel)
			require.NotNil(t, usageRepo.lastLog.UpstreamModelMismatch)
			require.True(t, *usageRepo.lastLog.UpstreamModelMismatch)
		})
	}
}

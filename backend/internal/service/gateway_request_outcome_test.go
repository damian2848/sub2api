//go:build unit

package service

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestGatewayRequestOutcomeIndependentOfCustomerCost(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		ws, incomplete, cyber bool
		terminal              string
		success               bool
	}{
		{name: "free success", success: true},
		{name: "charged partial stream", incomplete: true},
		{name: "ws success", ws: true, terminal: "response.completed", success: true},
		{name: "ws failed", ws: true, terminal: "response.failed"},
		{name: "quality policy rejected", cyber: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			result := &OpenAIForwardResult{RequestID: "outcome-" + tc.name, Model: "gpt-5.1", Duration: time.Second, OpenAIWSMode: tc.ws, UpstreamTerminalEvent: tc.terminal, streamReadIncomplete: tc.incomplete}
			require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: result, APIKey: &APIKey{ID: 2}, User: &User{ID: 1}, Account: &Account{ID: 3}, CyberBlocked: tc.cyber}))
			require.NotNil(t, repo.lastLog)
			require.NotNil(t, repo.lastLog.APISuccess)
			require.Equal(t, tc.success, *repo.lastLog.APISuccess)
			require.Zero(t, repo.lastLog.ActualCost)
		})
	}
}

func TestGatewayRequestOutcomeGenericPartialStream(t *testing.T) {
	for _, partial := range []bool{false, true} {
		repo := &openAIRecordUsageLogRepoStub{inserted: true}
		svc := newGatewayRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
		result := &ForwardResult{RequestID: "generic-outcome", Model: "claude-sonnet-4", streamReadIncomplete: partial}
		require.NoError(t, svc.RecordUsage(context.Background(), &RecordUsageInput{Result: result, APIKey: &APIKey{ID: 2}, User: &User{ID: 1}, Account: &Account{ID: 3}}))
		require.NotNil(t, repo.lastLog.APISuccess)
		require.Equal(t, !partial, *repo.lastLog.APISuccess)
	}
}

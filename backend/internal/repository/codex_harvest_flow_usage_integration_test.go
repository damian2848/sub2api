//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestHarvestFlowTicketUsageSurvivesRepositoryReload(t *testing.T) {
	testEntClient(t)
	ctx := t.Context()
	id := "usage-" + uuid.NewString()
	legacyID := "legacy-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM codex_harvest_flow_events WHERE event_id IN ($1,$2)`, id, legacyID)
	})
	at := time.Now().UTC().Truncate(time.Microsecond)
	_, err := integrationDB.ExecContext(ctx, `INSERT INTO codex_harvest_flow_events (event_id, at, stage, kind) VALUES ($1,$2,'ticket','accept')`, legacyID, at.Add(-time.Second))
	require.NoError(t, err)
	event := service.CodexHarvestFlowEvent{ID: id, At: at, Stage: "usage", Kind: "ticket_used", AccountID: 71, AccountName: "consumer", Model: "gpt-6-astra", HTTPStatus: 401, TicketUsage: &service.CodexTicketUsage{SourceAccountID: 88, SourceAccountName: "donor", Acquisition: "reuse", Gateway: "unified-42", Transport: "sse", RequestKind: "business"}}
	require.NoError(t, NewCodexHarvestFlowRepository(integrationDB).Append(ctx, event))
	require.NoError(t, NewCodexHarvestFlowRepository(integrationDB).Append(ctx, event))
	events, err := NewCodexHarvestFlowRepository(integrationDB).List(ctx, 200)
	require.NoError(t, err)
	found, legacy := 0, false
	for _, got := range events {
		if got.ID == id {
			found++
			require.Equal(t, event, got)
		}
		if got.ID == legacyID {
			legacy = true
			require.Nil(t, got.TicketUsage)
		}
	}
	require.Equal(t, 1, found)
	require.True(t, legacy)
}

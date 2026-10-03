package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProbeRequestFactsMigrationSeparatesCustomerBilling(t *testing.T) {
	content, err := FS.ReadFile("265_probe_request_facts.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS probe_request_facts")
	require.Contains(t, sql, "attempt_id TEXT NOT NULL UNIQUE")
	require.Contains(t, sql, "ON probe_request_facts (logical_request_id) WHERE is_final")
	require.Contains(t, sql, "role IN ('generation', 'quality_judge', 'account_check', 'state_probe')")
	require.Contains(t, sql, "first_token_ms BIGINT CHECK")
	require.Contains(t, sql, "usage_complete BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "cost_complete BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "'timeout'")
	require.NotContains(t, sql, "user_id BIGINT")
	require.NotContains(t, sql, "api_key_id BIGINT")
	require.NotContains(t, sql, "UPDATE users")
}

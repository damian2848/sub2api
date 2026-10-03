-- Internal probes bypass customer billing. Keep their actual upstream attempts in
-- a separate ledger; never synthesize users/API keys or replay customer debits.
CREATE TABLE IF NOT EXISTS probe_request_facts (
    id BIGSERIAL PRIMARY KEY,
    run_id TEXT NOT NULL,
    logical_request_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL UNIQUE,
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    is_final BOOLEAN NOT NULL DEFAULT FALSE,
    role TEXT NOT NULL CHECK (role IN ('generation', 'quality_judge', 'account_check', 'state_probe')),
    platform TEXT NOT NULL,
    group_id BIGINT,
    account_id BIGINT NOT NULL,
    protocol TEXT NOT NULL,
    requested_model TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL,
    duration_ms BIGINT NOT NULL CHECK (duration_ms >= 0),
    first_token_ms BIGINT CHECK (first_token_ms >= 0),
    http_status INTEGER,
    api_success BOOLEAN NOT NULL,
    error_kind TEXT CHECK (error_kind IN ('transport', 'http', 'stream', 'timeout')),
    input_tokens BIGINT,
    output_tokens BIGINT,
    cache_read_tokens BIGINT,
    cache_creation_tokens BIGINT,
    cache_creation_5m_tokens BIGINT,
    cache_creation_1h_tokens BIGINT,
    reasoning_tokens BIGINT,
    usage_complete BOOLEAN NOT NULL DEFAULT FALSE,
    upstream_cost_usd NUMERIC(20, 10),
    cost_complete BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (finished_at >= started_at),
    CHECK (group_id IS NULL OR group_id > 0)
);
CREATE INDEX IF NOT EXISTS probe_request_facts_channel_window_idx
    ON probe_request_facts (started_at, group_id, platform, requested_model)
    WHERE group_id IS NOT NULL AND role = 'generation';
CREATE INDEX IF NOT EXISTS probe_request_facts_account_window_idx
    ON probe_request_facts (account_id, started_at DESC);
CREATE INDEX IF NOT EXISTS probe_request_facts_logical_idx
    ON probe_request_facts (logical_request_id, attempt_number);
-- At most one final channel outcome per logical generation request; earlier
-- retry costs remain available without inflating the availability denominator.
CREATE UNIQUE INDEX IF NOT EXISTS probe_request_facts_final_idx
    ON probe_request_facts (logical_request_id) WHERE is_final;

CREATE INDEX IF NOT EXISTS probe_request_facts_finished_idx ON probe_request_facts (finished_at);

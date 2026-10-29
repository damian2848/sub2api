-- A quota cycle authorizes one durable cleanup. Retain events after completion
-- so restarts and concurrent quota readers cannot repeat that authorization.
CREATE TABLE IF NOT EXISTS gemini_web_history_cleanup_jobs (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    identity TEXT NOT NULL,
    windows JSONB NOT NULL CHECK (jsonb_typeof(windows) = 'array'),
    snapshot_ids JSONB CHECK (snapshot_ids IS NULL OR jsonb_typeof(snapshot_ids) = 'array'),
	conversation_count INTEGER NOT NULL DEFAULT 0 CHECK (conversation_count >= 0),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'completed', 'cancelled')),
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    last_error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    CHECK ((status = 'running') = (lease_token IS NOT NULL AND lease_until IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS gemini_web_history_cleanup_due
    ON gemini_web_history_cleanup_jobs (next_attempt_at, id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS gemini_web_history_cleanup_running
    ON gemini_web_history_cleanup_jobs (account_id, lease_until) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS gemini_web_history_cleanup_merge
    ON gemini_web_history_cleanup_jobs (account_id, identity, id)
    WHERE status = 'pending' AND snapshot_ids IS NULL;

CREATE TABLE IF NOT EXISTS gemini_web_history_cleanup_events (
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    window_id TEXT NOT NULL CHECK (window_id IN ('five_hour', 'weekly')),
    reset_at TIMESTAMPTZ NOT NULL,
    job_id BIGINT NOT NULL REFERENCES gemini_web_history_cleanup_jobs(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (account_id, window_id, reset_at)
);

-- These leases cover generation entry points, including administrative tests.
-- Admission and job claims serialize on the same account advisory lock.
CREATE TABLE IF NOT EXISTS gemini_web_history_requests (
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    token TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (account_id, token)
);
CREATE INDEX IF NOT EXISTS gemini_web_history_requests_expiry
    ON gemini_web_history_requests (account_id, expires_at);

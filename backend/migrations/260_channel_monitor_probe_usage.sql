-- Meter synthetic probes independently from customer traffic. Legacy usage stays unknown.
ALTER TABLE channel_monitor_v2_candy_results
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'probe' CHECK (source = 'probe'),
    ADD COLUMN IF NOT EXISTS platform VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS account_id BIGINT,
    ADD COLUMN IF NOT EXISTS requested_model VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS upstream_model VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    ADD COLUMN IF NOT EXISTS metering JSONB,
    ADD COLUMN IF NOT EXISTS probe_attempts JSONB NOT NULL DEFAULT '[]'::jsonb;

DROP INDEX IF EXISTS idx_channel_monitor_v2_candy_running;
CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_monitor_v2_candy_running
    ON channel_monitor_v2_candy_results (group_id, model, reasoning_effort) WHERE verdict = 'running';

CREATE INDEX IF NOT EXISTS idx_channel_monitor_v2_candy_dimension_history
    ON channel_monitor_v2_candy_results (group_id, model, platform, checked_at DESC);

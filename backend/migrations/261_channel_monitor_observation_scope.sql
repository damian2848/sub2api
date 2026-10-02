-- Bind external probes explicitly and retain the scope at execution time.
ALTER TABLE channel_monitors ADD COLUMN IF NOT EXISTS group_id BIGINT REFERENCES groups(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_channel_monitors_group_id ON channel_monitors(group_id);
ALTER TABLE channel_monitor_histories ADD COLUMN IF NOT EXISTS metering JSONB;
ALTER TABLE channel_monitor_histories ADD COLUMN IF NOT EXISTS observation_scope JSONB;
ALTER TABLE scheduled_test_results ADD COLUMN IF NOT EXISTS metering JSONB;
ALTER TABLE scheduled_test_results ADD COLUMN IF NOT EXISTS observation_scope JSONB;
CREATE INDEX IF NOT EXISTS idx_scheduled_test_results_observation_scope ON scheduled_test_results USING GIN(observation_scope);

CREATE TABLE IF NOT EXISTS channel_monitor_state_probe_results (
    id BIGSERIAL PRIMARY KEY,
    model VARCHAR(200) NOT NULL,
    verdict VARCHAR(32) NOT NULL CHECK (verdict IN ('healthy','degraded','inconclusive')),
    checked_at TIMESTAMPTZ NOT NULL,
    latency_ms BIGINT NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    metering JSONB,
    observation_scope JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_channel_monitor_state_probe_checked ON channel_monitor_state_probe_results(checked_at);

-- Monitoring history outlives the per-plan UI's configurable result limit.
CREATE TABLE IF NOT EXISTS channel_monitor_quality_observations (
    result_id BIGINT PRIMARY KEY,
    plan_id BIGINT NOT NULL,
    model VARCHAR(200) NOT NULL,
    type VARCHAR(32) NOT NULL,
    verdict VARCHAR(32) NOT NULL,
    checked_at TIMESTAMPTZ NOT NULL,
    latency_ms BIGINT NOT NULL DEFAULT 0,
    message TEXT NOT NULL DEFAULT '',
    schedule TEXT NOT NULL DEFAULT '',
    metering JSONB,
    observation_scope JSONB
);
CREATE INDEX IF NOT EXISTS idx_channel_monitor_quality_observation_checked ON channel_monitor_quality_observations(checked_at);

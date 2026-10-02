ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'business'
    CHECK (source IN ('business', 'probe'));
ALTER TABLE ops_error_logs
    ADD COLUMN IF NOT EXISTS source VARCHAR(16) NOT NULL DEFAULT 'business'
    CHECK (source IN ('business', 'probe'));

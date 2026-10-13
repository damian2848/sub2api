-- Optional, redacted provenance for actual ticket-use events; legacy events
-- remain readable. No ticket state, cookies, tokens or proxy credentials.
ALTER TABLE codex_harvest_flow_events
    ADD COLUMN IF NOT EXISTS ticket_usage JSONB;

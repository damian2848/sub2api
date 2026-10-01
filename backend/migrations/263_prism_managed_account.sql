CREATE UNIQUE INDEX IF NOT EXISTS accounts_prism_source_unique
ON accounts ((extra->>'prism_source_account_id'))
WHERE deleted_at IS NULL AND extra->>'provider_preset' = 'prism_browser';

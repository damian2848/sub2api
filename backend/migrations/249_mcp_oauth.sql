-- OAuth clients use public-client authorization code + S256 PKCE. No client
-- secrets, authorization codes, access tokens, or refresh tokens are stored raw.
CREATE TABLE IF NOT EXISTS mcp_oauth_clients (
    client_id VARCHAR(64) PRIMARY KEY,
    client_name VARCHAR(100) NOT NULL,
    redirect_uris JSONB NOT NULL,
    grant_types JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_clients_created ON mcp_oauth_clients(created_at);

CREATE TABLE IF NOT EXISTS mcp_oauth_grants (
    id VARCHAR(43) PRIMARY KEY,
    client_id VARCHAR(64) NOT NULL REFERENCES mcp_oauth_clients(client_id) ON DELETE RESTRICT,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_version BIGINT NOT NULL,
    resource TEXT NOT NULL,
    scope VARCHAR(16) NOT NULL CHECK (scope IN ('user', 'admin')),
    read_only BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_grants_user_created ON mcp_oauth_grants(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_grants_client ON mcp_oauth_grants(client_id);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_grants_expiry ON mcp_oauth_grants(expires_at);

CREATE TABLE IF NOT EXISTS mcp_oauth_codes (
    code_hash VARCHAR(64) PRIMARY KEY,
    grant_id VARCHAR(43) NOT NULL REFERENCES mcp_oauth_grants(id) ON DELETE CASCADE,
    client_id VARCHAR(64) NOT NULL,
    redirect_uri TEXT NOT NULL,
    resource TEXT NOT NULL,
    code_challenge VARCHAR(43) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_codes_grant ON mcp_oauth_codes(grant_id);

-- Consumed refresh digests remain until the grant is pruned, allowing a replay
-- to revoke the entire family, including access tokens issued after rotation.
CREATE TABLE IF NOT EXISTS mcp_oauth_credentials (
    key_hash VARCHAR(64) PRIMARY KEY,
    grant_id VARCHAR(43) NOT NULL REFERENCES mcp_oauth_grants(id) ON DELETE CASCADE,
    kind VARCHAR(8) NOT NULL CHECK (kind IN ('access', 'refresh')),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_credentials_grant ON mcp_oauth_credentials(grant_id);

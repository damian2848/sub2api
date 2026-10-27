package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type mcpOAuthRepository struct{ db *sql.DB }

func NewMCPOAuthRepository(db *sql.DB) service.MCPOAuthRepository {
	return &mcpOAuthRepository{db: db}
}

func (r *mcpOAuthRepository) CreateClient(ctx context.Context, client *service.MCPOAuthClient) error {
	redirects, err := json.Marshal(client.RedirectURIs)
	if err != nil {
		return err
	}
	grants, err := json.Marshal(client.GrantTypes)
	if err != nil {
		return err
	}
	// Registration also performs bounded retention work. Active authorizations
	// protect their client registration; used refresh hashes remain for the
	// entire grant lifetime so replay detection is never shortened by cleanup.
	now := time.Unix(client.ClientIDIssuedAt, 0).UTC()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_oauth_grants WHERE id IN (SELECT id FROM mcp_oauth_grants WHERE expires_at<$1 ORDER BY expires_at LIMIT 100 FOR UPDATE SKIP LOCKED)`, now.Add(-7*24*time.Hour)); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.client_id FROM mcp_oauth_clients c WHERE c.created_at<$1 AND NOT EXISTS (SELECT 1 FROM mcp_oauth_grants g WHERE g.client_id=c.client_id) ORDER BY c.created_at LIMIT 100 FOR UPDATE SKIP LOCKED`, now.Add(-90*24*time.Hour))
	if err != nil {
		return err
	}
	var staleClientIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		staleClientIDs = append(staleClientIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(staleClientIDs) != 0 {
		// The second statement gets a fresh READ COMMITTED snapshot after the
		// client rows are locked. A concurrent consent that committed just before
		// those locks must protect its newly inserted grant from client cleanup.
		if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_oauth_clients c WHERE c.client_id=ANY($1) AND NOT EXISTS (SELECT 1 FROM mcp_oauth_grants g WHERE g.client_id=c.client_id)`, pq.Array(staleClientIDs)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_oauth_clients (client_id,client_name,redirect_uris,grant_types,created_at) VALUES ($1,$2,$3,$4,$5)`, client.ClientID, client.ClientName, string(redirects), string(grants), now); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *mcpOAuthRepository) GetClient(ctx context.Context, id string) (*service.MCPOAuthClient, error) {
	client := &service.MCPOAuthClient{TokenEndpointAuthMethod: "none", ResponseTypes: []string{"code"}}
	var redirects, grants []byte
	var createdAt time.Time
	err := r.db.QueryRowContext(ctx, `SELECT client_id,client_name,redirect_uris,grant_types,created_at FROM mcp_oauth_clients WHERE client_id=$1`, id).
		Scan(&client.ClientID, &client.ClientName, &redirects, &grants, &createdAt)
	if err != nil {
		return nil, mcpOAuthSQLError(err)
	}
	if err := json.Unmarshal(redirects, &client.RedirectURIs); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(grants, &client.GrantTypes); err != nil {
		return nil, err
	}
	client.ClientIDIssuedAt = createdAt.Unix()
	return client, nil
}

func (r *mcpOAuthRepository) CreateAuthorization(ctx context.Context, grant *service.MCPOAuthGrant, code *service.MCPOAuthCode, limit int) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize the per-user limit with other authorizations for this owner.
	var ownerID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, grant.UserID).Scan(&ownerID); err != nil {
		return mcpOAuthSQLError(err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_oauth_grants WHERE user_id=$1 AND revoked_at IS NULL AND expires_at>$2`, grant.UserID, grant.CreatedAt).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return service.ErrMCPOAuthGrantLimit
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mcp_oauth_grants (id,client_id,user_id,token_version,resource,scope,read_only,created_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		grant.ID, grant.ClientID, grant.UserID, grant.TokenVersion, grant.Resource, grant.Scope, grant.ReadOnly, grant.CreatedAt, grant.ExpiresAt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mcp_oauth_codes (code_hash,grant_id,client_id,redirect_uri,resource,code_challenge,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		code.Hash, code.GrantID, code.ClientID, code.RedirectURI, code.Resource, code.CodeChallenge, code.ExpiresAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *mcpOAuthRepository) GetCode(ctx context.Context, hash string) (*service.MCPOAuthCode, error) {
	code := &service.MCPOAuthCode{}
	err := r.db.QueryRowContext(ctx, `SELECT code_hash,grant_id,client_id,redirect_uri,resource,code_challenge,expires_at,consumed_at FROM mcp_oauth_codes WHERE code_hash=$1`, hash).
		Scan(&code.Hash, &code.GrantID, &code.ClientID, &code.RedirectURI, &code.Resource, &code.CodeChallenge, &code.ExpiresAt, &code.ConsumedAt)
	if err != nil {
		return nil, mcpOAuthSQLError(err)
	}
	return code, nil
}

func (r *mcpOAuthRepository) GetCredential(ctx context.Context, hash string) (*service.MCPOAuthCredential, error) {
	credential := &service.MCPOAuthCredential{}
	err := r.db.QueryRowContext(ctx, `SELECT key_hash,grant_id,kind,expires_at,used_at FROM mcp_oauth_credentials WHERE key_hash=$1`, hash).
		Scan(&credential.Hash, &credential.GrantID, &credential.Kind, &credential.ExpiresAt, &credential.UsedAt)
	if err != nil {
		return nil, mcpOAuthSQLError(err)
	}
	return credential, nil
}

const mcpOAuthGrantColumns = `g.id,g.client_id,c.client_name,g.user_id,g.token_version,g.resource,g.scope,g.read_only,g.created_at,g.expires_at,g.last_used_at,g.revoked_at`

func scanMCPOAuthGrant(row interface{ Scan(...any) error }) (*service.MCPOAuthGrant, error) {
	grant := &service.MCPOAuthGrant{}
	err := row.Scan(&grant.ID, &grant.ClientID, &grant.ClientName, &grant.UserID, &grant.TokenVersion, &grant.Resource, &grant.Scope, &grant.ReadOnly, &grant.CreatedAt, &grant.ExpiresAt, &grant.LastUsedAt, &grant.RevokedAt)
	if err != nil {
		return nil, mcpOAuthSQLError(err)
	}
	return grant, nil
}

func (r *mcpOAuthRepository) GetGrant(ctx context.Context, id string) (*service.MCPOAuthGrant, error) {
	return scanMCPOAuthGrant(r.db.QueryRowContext(ctx, `SELECT `+mcpOAuthGrantColumns+` FROM mcp_oauth_grants g JOIN mcp_oauth_clients c ON c.client_id=g.client_id WHERE g.id=$1`, id))
}

func (r *mcpOAuthRepository) ListGrants(ctx context.Context, userID int64) ([]*service.MCPOAuthGrant, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+mcpOAuthGrantColumns+` FROM mcp_oauth_grants g JOIN mcp_oauth_clients c ON c.client_id=g.client_id WHERE g.user_id=$1 ORDER BY g.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	grants := []*service.MCPOAuthGrant{}
	for rows.Next() {
		grant, err := scanMCPOAuthGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (r *mcpOAuthRepository) ExchangeCode(ctx context.Context, hash, grantID string, at time.Time, access, refresh *service.MCPOAuthCredential) error {
	return r.consumeAndIssue(ctx, hash, grantID, at, access, refresh, false)
}

func (r *mcpOAuthRepository) RotateRefresh(ctx context.Context, hash, grantID string, at time.Time, access, refresh *service.MCPOAuthCredential) error {
	return r.consumeAndIssue(ctx, hash, grantID, at, access, refresh, true)
}

func (r *mcpOAuthRepository) consumeAndIssue(ctx context.Context, hash, grantID string, at time.Time, access, refresh *service.MCPOAuthCredential, rotating bool) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Always lock the grant first: rotation, code exchange, and revocation
	// serialize on the same family without code/refresh lock-order inversion.
	var revokedAt *time.Time
	var grantExpiry time.Time
	if err := tx.QueryRowContext(ctx, `SELECT revoked_at,expires_at FROM mcp_oauth_grants WHERE id=$1 FOR UPDATE`, grantID).Scan(&revokedAt, &grantExpiry); err != nil {
		return mcpOAuthSQLError(err)
	}
	if revokedAt != nil || !at.Before(grantExpiry) {
		return service.ErrMCPOAuthNotFound
	}
	var expiresAt time.Time
	var usedAt *time.Time
	query := `SELECT expires_at,consumed_at FROM mcp_oauth_codes WHERE code_hash=$1 AND grant_id=$2 FOR UPDATE`
	if rotating {
		query = `SELECT expires_at,used_at FROM mcp_oauth_credentials WHERE key_hash=$1 AND grant_id=$2 AND kind='refresh' FOR UPDATE`
	}
	if err := tx.QueryRowContext(ctx, query, hash, grantID).Scan(&expiresAt, &usedAt); err != nil {
		return mcpOAuthSQLError(err)
	}
	if usedAt != nil {
		// Do not return before committing: rolling back here would leave all
		// newer credentials usable after a detected replay.
		if _, err := tx.ExecContext(ctx, `UPDATE mcp_oauth_grants SET revoked_at=$2 WHERE id=$1`, grantID, at); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return service.ErrMCPOAuthNotFound
	}
	if !at.Before(expiresAt) {
		return service.ErrMCPOAuthNotFound
	}
	query = `UPDATE mcp_oauth_codes SET consumed_at=$2 WHERE code_hash=$1`
	if rotating {
		query = `UPDATE mcp_oauth_credentials SET used_at=$2 WHERE key_hash=$1`
	}
	if _, err := tx.ExecContext(ctx, query, hash, at); err != nil {
		return err
	}
	for _, credential := range []*service.MCPOAuthCredential{access, refresh} {
		if credential == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_oauth_credentials (key_hash,grant_id,kind,expires_at) VALUES ($1,$2,$3,$4)`, credential.Hash, grantID, credential.Kind, credential.ExpiresAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *mcpOAuthRepository) TouchGrant(ctx context.Context, id string, at time.Time) error {
	return mcpOAuthResult(r.db.ExecContext(ctx, `UPDATE mcp_oauth_grants SET last_used_at=$2 WHERE id=$1 AND revoked_at IS NULL AND expires_at>$2`, id, at))
}

func (r *mcpOAuthRepository) RevokeGrant(ctx context.Context, userID int64, id string, at time.Time) error {
	return mcpOAuthResult(r.db.ExecContext(ctx, `UPDATE mcp_oauth_grants SET revoked_at=COALESCE(revoked_at,$3) WHERE id=$1 AND user_id=$2`, id, userID, at))
}

func mcpOAuthSQLError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrMCPOAuthNotFound
	}
	return err
}

func mcpOAuthResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return service.ErrMCPOAuthNotFound
	}
	return nil
}

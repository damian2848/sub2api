//go:build unit

package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type mcpOAuthMemoryRepo struct {
	mu          sync.Mutex
	clients     map[string]*MCPOAuthClient
	grants      map[string]*MCPOAuthGrant
	codes       map[string]*MCPOAuthCode
	credentials map[string]*MCPOAuthCredential
}

func (r *mcpOAuthMemoryRepo) CreateClient(_ context.Context, client *MCPOAuthClient) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[client.ClientID] = client
	return nil
}

func (r *mcpOAuthMemoryRepo) GetClient(_ context.Context, id string) (*MCPOAuthClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if client := r.clients[id]; client != nil {
		copy := *client
		return &copy, nil
	}
	return nil, ErrMCPOAuthNotFound
}

func (r *mcpOAuthMemoryRepo) CreateAuthorization(_ context.Context, grant *MCPOAuthGrant, code *MCPOAuthCode, limit int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, existing := range r.grants {
		if existing.UserID == grant.UserID && existing.RevokedAt == nil && existing.ExpiresAt.After(grant.CreatedAt) {
			count++
		}
	}
	if count >= limit {
		return ErrMCPOAuthGrantLimit
	}
	r.grants[grant.ID], r.codes[code.Hash] = grant, code
	return nil
}

func (r *mcpOAuthMemoryRepo) GetCode(_ context.Context, hash string) (*MCPOAuthCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if code := r.codes[hash]; code != nil {
		copy := *code
		return &copy, nil
	}
	return nil, ErrMCPOAuthNotFound
}

func (r *mcpOAuthMemoryRepo) GetCredential(_ context.Context, hash string) (*MCPOAuthCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if credential := r.credentials[hash]; credential != nil {
		copy := *credential
		return &copy, nil
	}
	return nil, ErrMCPOAuthNotFound
}

func (r *mcpOAuthMemoryRepo) GetGrant(_ context.Context, id string) (*MCPOAuthGrant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if grant := r.grants[id]; grant != nil {
		copy := *grant
		return &copy, nil
	}
	return nil, ErrMCPOAuthNotFound
}

func (r *mcpOAuthMemoryRepo) consume(hash, grantID string, now time.Time, access, refresh *MCPOAuthCredential, rotating bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	grant := r.grants[grantID]
	if grant == nil || grant.RevokedAt != nil || !now.Before(grant.ExpiresAt) {
		return ErrMCPOAuthNotFound
	}
	var expiry time.Time
	var used **time.Time
	if rotating {
		old := r.credentials[hash]
		if old == nil || old.GrantID != grantID || old.Kind != "refresh" {
			return ErrMCPOAuthNotFound
		}
		expiry, used = old.ExpiresAt, &old.UsedAt
	} else {
		code := r.codes[hash]
		if code == nil || code.GrantID != grantID {
			return ErrMCPOAuthNotFound
		}
		expiry, used = code.ExpiresAt, &code.ConsumedAt
	}
	if *used != nil {
		grant.RevokedAt = &now
		return ErrMCPOAuthNotFound
	}
	if !now.Before(expiry) {
		return ErrMCPOAuthNotFound
	}
	*used = &now
	r.credentials[access.Hash] = access
	if refresh != nil {
		r.credentials[refresh.Hash] = refresh
	}
	return nil
}

func (r *mcpOAuthMemoryRepo) ExchangeCode(_ context.Context, hash, grantID string, now time.Time, access, refresh *MCPOAuthCredential) error {
	return r.consume(hash, grantID, now, access, refresh, false)
}

func (r *mcpOAuthMemoryRepo) RotateRefresh(_ context.Context, hash, grantID string, now time.Time, access, refresh *MCPOAuthCredential) error {
	return r.consume(hash, grantID, now, access, refresh, true)
}

func (r *mcpOAuthMemoryRepo) TouchGrant(_ context.Context, id string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	grant := r.grants[id]
	if grant == nil || grant.RevokedAt != nil || !now.Before(grant.ExpiresAt) {
		return ErrMCPOAuthNotFound
	}
	grant.LastUsedAt = &now
	return nil
}

func (r *mcpOAuthMemoryRepo) ListGrants(_ context.Context, userID int64) ([]*MCPOAuthGrant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	grants := []*MCPOAuthGrant{}
	for _, grant := range r.grants {
		if grant.UserID == userID {
			copy := *grant
			grants = append(grants, &copy)
		}
	}
	return grants, nil
}

func (r *mcpOAuthMemoryRepo) RevokeGrant(_ context.Context, userID int64, id string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	grant := r.grants[id]
	if grant == nil || grant.UserID != userID {
		return ErrMCPOAuthNotFound
	}
	if grant.RevokedAt == nil {
		grant.RevokedAt = &now
	}
	return nil
}

type mcpOAuthUserRepo struct {
	UserRepository
	user *User
}

func (r *mcpOAuthUserRepo) GetByID(_ context.Context, id int64) (*User, error) {
	if r.user == nil || r.user.ID != id {
		return nil, ErrUserNotFound
	}
	copy := *r.user
	return &copy, nil
}

const mcpTestResource = "https://platform.example/api/v1/mcp"

type mcpOAuthFixture struct {
	s         *MCPOAuthService
	r         *mcpOAuthMemoryRepo
	u         *mcpOAuthUserRepo
	client    *MCPOAuthClient
	authorize MCPOAuthAuthorizeInput
	verifier  string
}

func newMCPOAuthFixture(t *testing.T) *mcpOAuthFixture {
	t.Helper()
	r := &mcpOAuthMemoryRepo{clients: map[string]*MCPOAuthClient{}, grants: map[string]*MCPOAuthGrant{}, codes: map[string]*MCPOAuthCode{}, credentials: map[string]*MCPOAuthCredential{}}
	u := &mcpOAuthUserRepo{user: &User{ID: 7, Role: RoleUser, Status: StatusActive, Email: "user@example.com", PasswordHash: "original", Concurrency: 3}}
	s := NewMCPOAuthService(r, u)
	client, err := s.Register(context.Background(), MCPOAuthRegisterInput{ClientName: "My MCP client", RedirectURIs: []string{"http://127.0.0.1:2345/callback?existing=1", "https://client.example/callback"}})
	require.NoError(t, err)
	verifier := strings.Repeat("a", 43)
	sum := sha256.Sum256([]byte(verifier))
	return &mcpOAuthFixture{s: s, r: r, u: u, client: client, verifier: verifier,
		authorize: MCPOAuthAuthorizeInput{ResponseType: "code", ClientID: client.ClientID, RedirectURI: client.RedirectURIs[0], Scope: "mcp", State: "opaque&state", CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]), CodeChallengeMethod: "S256", Resource: mcpTestResource}}
}

func (f *mcpOAuthFixture) code(t *testing.T, scope string, readOnly *bool) MCPOAuthTokenInput {
	t.Helper()
	redirect, err := f.s.Decide(context.Background(), 7, MCPOAuthConsentInput{Authorization: f.authorize, Approved: true, Scope: scope, ReadOnly: readOnly}, mcpTestResource)
	require.NoError(t, err)
	u, err := url.Parse(redirect)
	require.NoError(t, err)
	require.Equal(t, "opaque&state", u.Query().Get("state"))
	require.Equal(t, "1", u.Query().Get("existing"))
	return MCPOAuthTokenInput{GrantType: "authorization_code", ClientID: f.client.ClientID, Code: u.Query().Get("code"), RedirectURI: f.authorize.RedirectURI, CodeVerifier: f.verifier, Resource: mcpTestResource}
}

func (f *mcpOAuthFixture) tokens(t *testing.T) *MCPOAuthTokenResponse {
	t.Helper()
	tokens, err := f.s.Token(context.Background(), f.code(t, "", nil), mcpTestResource)
	require.NoError(t, err)
	return tokens
}

func requireMCPOAuthError(t *testing.T, err error, code string) {
	t.Helper()
	var oauthErr *MCPOAuthError
	require.ErrorAs(t, err, &oauthErr)
	require.Equal(t, code, oauthErr.Code)
}

func TestMCPOAuthRegistrationValidatesRedirectsAndPublicClientMetadata(t *testing.T) {
	f := newMCPOAuthFixture(t)
	require.Equal(t, "none", f.client.TokenEndpointAuthMethod)
	require.Equal(t, []string{"authorization_code", "refresh_token"}, f.client.GrantTypes)
	for _, uri := range []string{"https://client.example/callback", "http://localhost:3000/cb", "http://[::1]:3000/cb", "http://127.0.0.1/cb"} {
		_, err := f.s.Register(context.Background(), MCPOAuthRegisterInput{RedirectURIs: []string{uri}})
		require.NoError(t, err, uri)
	}
	for _, uri := range []string{"http://client.example/cb", "https://client.example/cb#fragment", "https://user:password@client.example/cb", "https://*.example/cb", "file:///callback", "https:///callback", "http://127.0.0.1.evil.example/cb", "https://client.example/cb#"} {
		_, err := f.s.Register(context.Background(), MCPOAuthRegisterInput{RedirectURIs: []string{uri}})
		requireMCPOAuthError(t, err, "invalid_redirect_uri")
	}
	_, err := f.s.Register(context.Background(), MCPOAuthRegisterInput{RedirectURIs: []string{"https://client.example/cb"}, TokenEndpointAuthMethod: "client_secret_post"})
	requireMCPOAuthError(t, err, "invalid_client_metadata")
}

func TestMCPOAuthAuthorizationRequiresPKCEResourceAndRegisteredRedirect(t *testing.T) {
	f := newMCPOAuthFixture(t)
	preview, err := f.s.Preview(context.Background(), 7, f.authorize, mcpTestResource)
	require.NoError(t, err)
	require.Equal(t, MCPOAuthScope, preview.Scope)
	for _, tc := range []struct {
		name, code string
		mutate     func(*MCPOAuthAuthorizeInput)
	}{
		{"redirect", "invalid_request", func(a *MCPOAuthAuthorizeInput) { a.RedirectURI += "x" }},
		{"client", "invalid_client", func(a *MCPOAuthAuthorizeInput) { a.ClientID = "unknown" }},
		{"response", "unsupported_response_type", func(a *MCPOAuthAuthorizeInput) { a.ResponseType = "token" }},
		{"scope", "invalid_scope", func(a *MCPOAuthAuthorizeInput) { a.Scope = "mcp admin" }},
		{"resource", "invalid_target", func(a *MCPOAuthAuthorizeInput) { a.Resource = "https://other.example/mcp" }},
		{"missing resource", "invalid_target", func(a *MCPOAuthAuthorizeInput) { a.Resource = "" }},
		{"plain PKCE", "invalid_request", func(a *MCPOAuthAuthorizeInput) { a.CodeChallengeMethod = "plain" }},
		{"missing PKCE", "invalid_request", func(a *MCPOAuthAuthorizeInput) { a.CodeChallenge = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := f.authorize
			tc.mutate(&input)
			_, err := f.s.Preview(context.Background(), 7, input, mcpTestResource)
			requireMCPOAuthError(t, err, tc.code)
		})
	}
	redirect, err := f.s.Decide(context.Background(), 7, MCPOAuthConsentInput{Authorization: f.authorize}, mcpTestResource)
	require.NoError(t, err)
	u, err := url.Parse(redirect)
	require.NoError(t, err)
	require.Equal(t, "access_denied", u.Query().Get("error"))
	require.Equal(t, f.authorize.State, u.Query().Get("state"))
	require.Empty(t, f.r.grants)
}

func TestMCPOAuthCodeBindingFailuresDoNotConsumeCode(t *testing.T) {
	for _, name := range []string{"client", "redirect", "resource", "missing resource", "verifier"} {
		t.Run(name, func(t *testing.T) {
			f := newMCPOAuthFixture(t)
			valid := f.code(t, "", nil)
			invalid := valid
			switch name {
			case "client":
				client, err := f.s.Register(context.Background(), MCPOAuthRegisterInput{RedirectURIs: f.client.RedirectURIs})
				require.NoError(t, err)
				invalid.ClientID = client.ClientID
			case "redirect":
				invalid.RedirectURI = f.client.RedirectURIs[1]
			case "resource":
				invalid.Resource = "https://other.example/mcp"
			case "missing resource":
				invalid.Resource = ""
			case "verifier":
				invalid.CodeVerifier = strings.Repeat("b", 43)
			}
			_, err := f.s.Token(context.Background(), invalid, mcpTestResource)
			requireMCPOAuthError(t, err, "invalid_grant")
			_, err = f.s.Token(context.Background(), valid, mcpTestResource)
			require.NoError(t, err)
		})
	}
}

func TestMCPOAuthCodeReplayRevokesEntireGrant(t *testing.T) {
	f := newMCPOAuthFixture(t)
	input := f.code(t, "", nil)
	tokens, err := f.s.Token(context.Background(), input, mcpTestResource)
	require.NoError(t, err)
	principal, err := f.s.Authenticate(context.Background(), tokens.AccessToken)
	require.NoError(t, err)
	require.Equal(t, MCPScopeUser, principal.Scope)
	require.True(t, principal.ReadOnly)
	require.Equal(t, mcpTestResource, principal.Resource)
	require.Equal(t, int64(600), tokens.ExpiresIn)
	_, err = f.s.Token(context.Background(), input, mcpTestResource)
	requireMCPOAuthError(t, err, "invalid_grant")
	_, err = f.s.Authenticate(context.Background(), tokens.AccessToken)
	require.ErrorIs(t, err, ErrMCPOAuthInvalidToken)
	_, err = f.s.Token(context.Background(), MCPOAuthTokenInput{GrantType: "refresh_token", ClientID: f.client.ClientID, RefreshToken: tokens.RefreshToken}, mcpTestResource)
	requireMCPOAuthError(t, err, "invalid_grant")
}

func TestMCPOAuthRefreshRotationInheritsResourceAndDetectsReplay(t *testing.T) {
	f := newMCPOAuthFixture(t)
	first := f.tokens(t)
	input := MCPOAuthTokenInput{GrantType: "refresh_token", ClientID: f.client.ClientID, RefreshToken: first.RefreshToken}
	second, err := f.s.Token(context.Background(), input, mcpTestResource)
	require.NoError(t, err)
	require.NotEqual(t, first.AccessToken, second.AccessToken)
	require.NotEqual(t, first.RefreshToken, second.RefreshToken)
	_, err = f.s.Authenticate(context.Background(), first.AccessToken)
	require.NoError(t, err, "ordinary refresh must preserve in-flight access credentials")
	_, err = f.s.Authenticate(context.Background(), second.AccessToken)
	require.NoError(t, err)
	require.Equal(t, f.r.credentials[hashMCPOAuthSecret(first.RefreshToken)].ExpiresAt, f.r.credentials[hashMCPOAuthSecret(second.RefreshToken)].ExpiresAt)
	_, err = f.s.Token(context.Background(), input, mcpTestResource)
	requireMCPOAuthError(t, err, "invalid_grant")
	_, err = f.s.Authenticate(context.Background(), second.AccessToken)
	require.ErrorIs(t, err, ErrMCPOAuthInvalidToken)
	input.RefreshToken = second.RefreshToken
	_, err = f.s.Token(context.Background(), input, mcpTestResource)
	requireMCPOAuthError(t, err, "invalid_grant")
}

func TestMCPOAuthRefreshRejectsOtherClientResourceAndScope(t *testing.T) {
	f := newMCPOAuthFixture(t)
	tokens := f.tokens(t)
	other, err := f.s.Register(context.Background(), MCPOAuthRegisterInput{RedirectURIs: f.client.RedirectURIs})
	require.NoError(t, err)
	for _, tc := range []struct{ client, resource, scope, code string }{
		{other.ClientID, "", "", "invalid_grant"},
		{f.client.ClientID, "https://other.example/mcp", "", "invalid_grant"},
		{f.client.ClientID, "", "admin", "invalid_scope"},
	} {
		_, err := f.s.Token(context.Background(), MCPOAuthTokenInput{GrantType: "refresh_token", ClientID: tc.client, RefreshToken: tokens.RefreshToken, Resource: tc.resource, Scope: tc.scope}, mcpTestResource)
		requireMCPOAuthError(t, err, tc.code)
	}
	_, err = f.s.Token(context.Background(), MCPOAuthTokenInput{GrantType: "refresh_token", ClientID: f.client.ClientID, RefreshToken: tokens.RefreshToken, Resource: mcpTestResource}, mcpTestResource)
	require.NoError(t, err)
}

func TestMCPOAuthRechecksLiveOwnerOnExchangeRefreshAndAccess(t *testing.T) {
	for _, operation := range []string{"exchange", "refresh", "access"} {
		for _, change := range []string{"password", "email", "disabled", "deleted", "missing", "demoted"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				f := newMCPOAuthFixture(t)
				f.u.user.Role = RoleAdmin
				code := f.code(t, MCPScopeAdmin, nil)
				var tokens *MCPOAuthTokenResponse
				if operation != "exchange" {
					var err error
					tokens, err = f.s.Token(context.Background(), code, mcpTestResource)
					require.NoError(t, err)
				}
				switch change {
				case "password":
					f.u.user.PasswordHash = "changed"
				case "email":
					f.u.user.Email = "new@example.com"
				case "disabled":
					f.u.user.Status = StatusDisabled
				case "deleted":
					now := time.Now()
					f.u.user.DeletedAt = &now
				case "missing":
					f.u.user = nil
				case "demoted":
					f.u.user.Role = RoleUser
				}
				switch operation {
				case "exchange":
					_, err := f.s.Token(context.Background(), code, mcpTestResource)
					requireMCPOAuthError(t, err, "invalid_grant")
				case "refresh":
					_, err := f.s.Token(context.Background(), MCPOAuthTokenInput{GrantType: "refresh_token", ClientID: f.client.ClientID, RefreshToken: tokens.RefreshToken}, mcpTestResource)
					requireMCPOAuthError(t, err, "invalid_grant")
				case "access":
					_, err := f.s.Authenticate(context.Background(), tokens.AccessToken)
					require.ErrorIs(t, err, ErrMCPOAuthInvalidToken)
				}
			})
		}
	}
}

func TestMCPOAuthExpiryAndCredentialTypeBoundaries(t *testing.T) {
	t.Run("code", func(t *testing.T) {
		f := newMCPOAuthFixture(t)
		input := f.code(t, "", nil)
		f.r.codes[hashMCPOAuthSecret(input.Code)].ExpiresAt = time.Now().Add(-time.Second)
		_, err := f.s.Token(context.Background(), input, mcpTestResource)
		requireMCPOAuthError(t, err, "invalid_grant")
	})
	for _, kind := range []string{"access", "refresh", "grant"} {
		t.Run(kind, func(t *testing.T) {
			f := newMCPOAuthFixture(t)
			tokens := f.tokens(t)
			principal, err := f.s.Authenticate(context.Background(), tokens.AccessToken)
			require.NoError(t, err)
			past := time.Now().Add(-time.Second)
			switch kind {
			case "access":
				f.r.credentials[hashMCPOAuthSecret(tokens.AccessToken)].ExpiresAt = past
			case "refresh":
				f.r.credentials[hashMCPOAuthSecret(tokens.RefreshToken)].ExpiresAt = past
			case "grant":
				f.r.grants[principal.GrantID].ExpiresAt = past
			}
			if kind != "refresh" {
				_, err = f.s.Authenticate(context.Background(), tokens.AccessToken)
				require.ErrorIs(t, err, ErrMCPOAuthInvalidToken)
			}
			if kind != "access" {
				_, err = f.s.Token(context.Background(), MCPOAuthTokenInput{GrantType: "refresh_token", ClientID: f.client.ClientID, RefreshToken: tokens.RefreshToken}, mcpTestResource)
				requireMCPOAuthError(t, err, "invalid_grant")
			}
		})
	}
	f := newMCPOAuthFixture(t)
	tokens := f.tokens(t)
	_, err := f.s.Authenticate(context.Background(), tokens.RefreshToken)
	require.ErrorIs(t, err, ErrMCPOAuthInvalidToken)
	_, err = f.s.Authenticate(context.Background(), "mcp_legacy_personal_token")
	require.ErrorIs(t, err, ErrMCPOAuthInvalidToken)
}

func TestMCPOAuthConsentPermissionsRevocationAndNoSecretPersistence(t *testing.T) {
	f := newMCPOAuthFixture(t)
	_, err := f.s.Decide(context.Background(), 7, MCPOAuthConsentInput{Authorization: f.authorize, Approved: true, Scope: MCPScopeAdmin}, mcpTestResource)
	requireMCPOAuthError(t, err, "access_denied")
	f.u.user.Role = RoleAdmin
	readOnly := false
	input := f.code(t, MCPScopeAdmin, &readOnly)
	tokens, err := f.s.Token(context.Background(), input, mcpTestResource)
	require.NoError(t, err)
	principal, err := f.s.Authenticate(context.Background(), tokens.AccessToken)
	require.NoError(t, err)
	require.Equal(t, MCPScopeAdmin, principal.Scope)
	require.False(t, principal.ReadOnly)
	grants, err := f.s.ListGrants(context.Background(), 7)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.NotNil(t, grants[0].LastUsedAt)
	encoded, err := json.Marshal(grants)
	require.NoError(t, err)
	for _, secret := range []string{input.Code, tokens.AccessToken, tokens.RefreshToken, "token_version", "user_id", "resource"} {
		require.NotContains(t, string(encoded), secret)
	}
	require.NotContains(t, f.r.credentials, tokens.AccessToken)
	require.NotContains(t, f.r.credentials, tokens.RefreshToken)
	require.NotContains(t, f.r.codes, input.Code)
	require.ErrorIs(t, f.s.RevokeGrant(context.Background(), 8, principal.GrantID), ErrMCPOAuthGrantNotFound)
	require.NoError(t, f.s.RevokeToken(context.Background(), "other-client", tokens.RefreshToken))
	_, err = f.s.Authenticate(context.Background(), tokens.AccessToken)
	require.NoError(t, err)
	require.NoError(t, f.s.RevokeGrant(context.Background(), 7, principal.GrantID))
	require.NoError(t, f.s.RevokeGrant(context.Background(), 7, principal.GrantID))
	_, err = f.s.Authenticate(context.Background(), tokens.AccessToken)
	require.ErrorIs(t, err, ErrMCPOAuthInvalidToken)
}

func TestMCPOAuthListReportsInvalidatedAndGrantLimit(t *testing.T) {
	f := newMCPOAuthFixture(t)
	for i := 0; i < MCPOAuthGrantLimit; i++ {
		f.code(t, "", nil)
	}
	_, err := f.s.Decide(context.Background(), 7, MCPOAuthConsentInput{Authorization: f.authorize, Approved: true}, mcpTestResource)
	require.ErrorIs(t, err, ErrMCPOAuthGrantLimit)
	f.u.user.PasswordHash = "changed"
	grants, err := f.s.ListGrants(context.Background(), 7)
	require.NoError(t, err)
	for _, grant := range grants {
		require.True(t, grant.Invalidated)
	}
}

func TestMCPOAuthClientRevocationInvalidatesAccessAndRefresh(t *testing.T) {
	f := newMCPOAuthFixture(t)
	tokens := f.tokens(t)
	require.NoError(t, f.s.RevokeToken(context.Background(), f.client.ClientID, tokens.RefreshToken))
	_, err := f.s.Authenticate(context.Background(), tokens.AccessToken)
	require.ErrorIs(t, err, ErrMCPOAuthInvalidToken)
	_, err = f.s.Token(context.Background(), MCPOAuthTokenInput{GrantType: "refresh_token", ClientID: f.client.ClientID, RefreshToken: tokens.RefreshToken}, mcpTestResource)
	requireMCPOAuthError(t, err, "invalid_grant")
	require.NoError(t, f.s.RevokeToken(context.Background(), f.client.ClientID, tokens.RefreshToken))
	require.NoError(t, f.s.RevokeToken(context.Background(), f.client.ClientID, "unknown"))
}

func TestMCPOAuthCodeOnlyClientReceivesNoRefreshToken(t *testing.T) {
	f := newMCPOAuthFixture(t)
	f.r.clients[f.client.ClientID].GrantTypes = []string{"authorization_code"}
	tokens := f.tokens(t)
	require.NotEmpty(t, tokens.AccessToken)
	require.Empty(t, tokens.RefreshToken)
	encoded, err := json.Marshal(tokens)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "refresh_token")
	require.Len(t, f.r.credentials, 1)
}

func TestMCPOAuthConcurrentCodeExchangeAllowsAtMostOneIssuance(t *testing.T) {
	f := newMCPOAuthFixture(t)
	input := f.code(t, "", nil)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := f.s.Token(context.Background(), input, mcpTestResource)
			results <- err
		}()
	}
	close(start)
	successes := 0
	for i := 0; i < 2; i++ {
		if err := <-results; err == nil {
			successes++
		} else {
			requireMCPOAuthError(t, err, "invalid_grant")
		}
	}
	require.Equal(t, 1, successes)
	require.Len(t, f.r.credentials, 2, "only one credential pair may be persisted")
}

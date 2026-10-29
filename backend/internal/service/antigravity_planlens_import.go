package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const AntigravityPlanlensImportCapability = "antigravity-planlens-import-v1"
const antigravityOAuthGeneration = "planlens_oauth_generation"

type AntigravityPlanlensImportRequest struct {
	Platform     string   `json:"platform"`
	Email        string   `json:"email"`
	Aliases      []string `json:"aliases"`
	ProxyURL     string   `json:"proxyUrl"`
	RemoteID     string   `json:"remoteId"`
	Revision     string   `json:"revision"`
	DeadlineAt   int64    `json:"deadlineAt"`
	OAuthAccount *struct {
		Platform    string         `json:"platform"`
		Type        string         `json:"type"`
		Credentials map[string]any `json:"credentials"`
	} `json:"oauthAccount"`
}
type AntigravityPlanlensImportResult struct {
	Capability string `json:"capability"`
	OK         bool   `json:"ok"`
	Status     string `json:"status"`
	Platform   string `json:"platform,omitempty"`
	Email      string `json:"email,omitempty"`
	RemoteID   string `json:"remoteId,omitempty"`
	Revision   string `json:"revision,omitempty"`
}

func antImportResult(status string) *AntigravityPlanlensImportResult {
	return &AntigravityPlanlensImportResult{Capability: AntigravityPlanlensImportCapability, OK: status == "ready" || status == "imported" || status == "updated", Status: status}
}

var antImportEmailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
var antImportModelPattern = regexp.MustCompile(`^[a-zA-Z0-9._:/+*\-]{1,200}$`)

func antImportEmail(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 254 || !antImportEmailPattern.MatchString(value) {
		return ""
	}
	return value
}
func antImportAccountEmail(a *Account) string {
	for _, value := range []string{a.GetCredential("email"), stringExtraEmail(a.Extra), a.Name} {
		if strings.TrimSpace(value) != "" {
			return antImportEmail(value)
		}
	}
	return ""
}
func stringExtraEmail(extra map[string]any) string { value, _ := extra["email"].(string); return value }
func antImportHash(value any) string {
	payload, _ := json.Marshal(value)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
func antImportToken(value any, max int) bool {
	token, ok := value.(string)
	return ok && len(token) > 0 && len(token) <= max && !strings.ContainsAny(token, " \t\r\n\x00")
}

// Only errors emitted by the native OAuth refresh path are eligible. An
// administrative disable and unrelated policy/permission failures remain intact.
func AntigravityPlanlensRecoverableOAuthError(a *Account, now time.Time) bool {
	if a == nil || a.Status != StatusError || !a.Schedulable {
		return false
	}
	for _, until := range []*time.Time{a.RateLimitResetAt, a.OverloadUntil, a.TempUnschedulableUntil} {
		if until != nil && until.After(now) {
			return false
		}
	}
	if a.ExpiresAt != nil && !a.ExpiresAt.After(now) {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(a.ErrorMessage))
	if !strings.HasPrefix(message, "token refresh failed (non-retryable):") {
		return false
	}
	for _, blocked := range []string{"invalid_client", "unauthorized_client", "permission", "policy", "terms", "tos violation", "suspend", "banned", "blocked", "account_disabled", "account disabled", "account has been disabled", "billing", "payment", "workspace", "organization", "abuse"} {
		if strings.Contains(message, blocked) {
			return false
		}
	}
	for _, auth := range []string{"invalid_grant", "invalid_refresh_token", "token_expired", "missing_project_id:"} {
		if strings.Contains(message, auth) {
			return true
		}
	}
	return false
}

func validateAntImport(req *AntigravityPlanlensImportRequest, commit bool) error {
	if req.Platform != PlatformAntigravity || antImportEmail(req.Email) == "" || len(req.Aliases) > 32 {
		return errors.New("invalid_request")
	}
	req.Email = antImportEmail(req.Email)
	aliases := map[string]bool{req.Email: true}
	for _, alias := range req.Aliases {
		value := antImportEmail(alias)
		if value == "" {
			return errors.New("invalid_request")
		}
		aliases[value] = true
	}
	req.Aliases = nil
	for alias := range aliases {
		req.Aliases = append(req.Aliases, alias)
	}
	sort.Strings(req.Aliases)
	if len(req.Aliases) > 32 {
		return errors.New("invalid_request")
	}
	if req.RemoteID != "" {
		id, err := strconv.ParseInt(req.RemoteID, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != req.RemoteID {
			return errors.New("invalid_request")
		}
	}
	if req.DeadlineAt <= time.Now().UnixMilli() || req.DeadlineAt > time.Now().Add(5*time.Minute).UnixMilli() {
		return errors.New("expired")
	}
	if _, err := antImportProxy(req.ProxyURL); err != nil {
		return err
	}
	if !commit {
		return nil
	}
	if len(req.Revision) != 64 {
		return errors.New("invalid_request")
	}
	if _, err := hex.DecodeString(req.Revision); err != nil {
		return errors.New("invalid_request")
	}
	a := req.OAuthAccount
	if a == nil || a.Platform != PlatformAntigravity || a.Type != AccountTypeOAuth || antImportEmail(stringExtraEmail(a.Credentials)) != req.Email {
		return errors.New("invalid_request")
	}
	c := a.Credentials
	if !antImportToken(c["access_token"], 16384) || !antImportToken(c["refresh_token"], 16384) || !antImportToken(c["project_id"], 256) || c["token_type"] != "Bearer" {
		return errors.New("invalid_request")
	}
	expiration, ok := c["expires_at"].(string)
	if !ok {
		return errors.New("invalid_request")
	}
	expiry, err := strconv.ParseInt(expiration, 10, 64)
	if err != nil || expiry <= time.Now().Unix() {
		return errors.New("expired")
	}
	if mapping, exists := c["model_mapping"]; exists {
		values, ok := mapping.(map[string]any)
		if !ok || len(values) > 256 {
			return errors.New("invalid_request")
		}
		for key, value := range values {
			model, ok := value.(string)
			if !ok || !antImportModelPattern.MatchString(key) || !antImportModelPattern.MatchString(model) {
				return errors.New("invalid_request")
			}
		}
	}
	return nil
}
func antImportProxy(raw string) (*Proxy, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 4096 || strings.ContainsAny(raw, " \t\r\n\x00") {
		return nil, errors.New("invalid_request")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid_request")
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return nil, errors.New("invalid_request")
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > 65535 {
		return nil, errors.New("invalid_request")
	}
	proxy := &Proxy{Name: "PlanLens manual import", Protocol: u.Scheme, Host: strings.ToLower(u.Hostname()), Port: number, Status: StatusActive, FallbackMode: FallbackModeNone, ExpiryWarnDays: 7}
	if u.User != nil {
		proxy.Username = u.User.Username()
		proxy.Password, _ = u.User.Password()
	}
	return proxy, nil
}
func antProxyIdentity(p *Proxy) string {
	if p == nil {
		return ""
	}
	return antImportHash([]any{p.Protocol, strings.ToLower(p.Host), p.Port, p.Username, p.Password})
}

type antImportTemplate struct {
	GroupID             int64
	Concurrency         int
	Priority            int
	RateMultiplier      *float64
	GroupRateMultiplier *float64
	LoadFactor          *int
	AutoPauseOnExpired  bool
	GroupPriority       int
	AllowedModels       []string
}
type antImportSnapshot struct {
	account  *Account
	template *antImportTemplate
	proxy    *Proxy
	revision string
}

func antImportConfig(a *Account) antImportTemplate {
	t := antImportTemplate{Concurrency: a.Concurrency, Priority: a.Priority, RateMultiplier: a.RateMultiplier, GroupRateMultiplier: a.GroupRateMultiplier, LoadFactor: a.LoadFactor, AutoPauseOnExpired: a.AutoPauseOnExpired}
	if len(a.AccountGroups) == 1 {
		t.GroupID = a.AccountGroups[0].GroupID
		t.GroupPriority = a.AccountGroups[0].Priority
		t.AllowedModels = append([]string(nil), a.AccountGroups[0].AllowedModels...)
		sort.Strings(t.AllowedModels)
	}
	return t
}

// Snapshot construction is also rerun under a transaction lock at commit. New
// accounts require one unambiguous non-personal group and a common configuration.
func antImportPrepare(ctx context.Context, accounts AccountRepository, groups GroupRepository, proxies ProxyRepository, req AntigravityPlanlensImportRequest) (*antImportSnapshot, error) {
	rows, err := accounts.ListAllWithFilters(ctx, PlatformAntigravity, AccountTypeOAuth, "", "", 0, "")
	if err != nil {
		return nil, err
	}
	aliases := map[string]bool{}
	for _, alias := range req.Aliases {
		aliases[alias] = true
	}
	snap := &antImportSnapshot{}
	for i := range rows {
		if aliases[antImportAccountEmail(&rows[i])] {
			if snap.account != nil {
				return nil, errors.New("ambiguous_identity")
			}
			snap.account = &rows[i]
		}
	}
	if req.RemoteID != "" && (snap.account == nil || strconv.FormatInt(snap.account.ID, 10) != req.RemoteID) {
		return nil, errors.New("identity_mismatch")
	}
	if snap.account != nil {
		a := snap.account
		// Read the complete row, including associations. Preserve every manual field.
		a, err = accounts.GetByID(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		snap.account = a
		snap.revision = antImportHash([]any{req.Email, req.Aliases, a.ID, a.Name, a.Credentials, a.Extra, a.ProxyID, antImportConfig(a), a.GroupIDs, a.Status, a.Schedulable, a.ErrorMessage, a.ExpiresAt, a.TempUnschedulableReason, a.TempUnschedulableUntil, a.RateLimitResetAt, a.OverloadUntil})
		return snap, nil
	}
	active, err := groups.ListActiveByPlatform(ctx, PlatformAntigravity)
	if err != nil {
		return nil, err
	}
	var main *Group
	for i := range active {
		if strings.EqualFold(strings.TrimSpace(active[i].Name), "Antigravity") {
			if main != nil {
				return nil, errors.New("import_configuration_missing")
			}
			main = &active[i]
		}
	}
	if main == nil {
		return nil, errors.New("import_configuration_missing")
	}
	for i := range rows {
		if len(rows[i].GroupIDs) != 1 || rows[i].GroupIDs[0] != main.ID {
			continue
		}
		account, err := accounts.GetByID(ctx, rows[i].ID)
		if err != nil {
			return nil, err
		}
		cfg := antImportConfig(account)
		if cfg.GroupID != main.ID || cfg.Concurrency < 1 || cfg.Priority < 0 {
			return nil, errors.New("import_configuration_missing")
		}
		if snap.template != nil && antImportHash(snap.template) != antImportHash(cfg) {
			return nil, errors.New("import_configuration_missing")
		}
		snap.template = &cfg
	}
	if snap.template == nil {
		return nil, errors.New("import_configuration_missing")
	}
	snap.proxy, err = antImportProxy(req.ProxyURL)
	if err != nil {
		return nil, err
	}
	if snap.proxy != nil {
		candidates, err := proxies.ListAllForFallback(ctx)
		if err != nil {
			return nil, err
		}
		matches := 0
		for i := range candidates {
			if antProxyIdentity(&candidates[i]) == antProxyIdentity(snap.proxy) {
				matches++
				if matches > 1 || !candidates[i].IsActive() || candidates[i].IsExpired(time.Now()) {
					return nil, errors.New("import_configuration_missing")
				}
				copy := candidates[i]
				snap.proxy = &copy
			}
		}
	}
	snap.revision = antImportHash([]any{req.Email, req.Aliases, snap.template, main.ID, main.Name, main.Status, snap.proxy})
	return snap, nil
}

type antImportTransactionRepository interface {
	WithAntigravityImportTransaction(context.Context, func(AccountRepository, GroupRepository, ProxyRepository) (int64, error)) error
}
type antImportWriter interface {
	ApplyAntigravityImportCredentials(context.Context, *Account, map[string]any, int64) error
	CreateWithAccountGroups(context.Context, *Account, []AccountGroup) error
}
type antigravityOAuthFailureRepository interface {
	SetAntigravityOAuthFailureIfCredentialsUnchanged(context.Context, *Account, time.Time, string, bool) (bool, error)
}

func antImportFailure(err error) *AntigravityPlanlensImportResult {
	if err == nil {
		return antImportResult("remote_error")
	}
	switch err.Error() {
	case "invalid_request", "expired", "busy", "capability_missing", "verification_failed", "identity_mismatch", "ambiguous_identity", "import_configuration_missing", "stale_revision":
		return antImportResult(err.Error())
	}
	return antImportResult("remote_error")
}
func (s *adminServiceImpl) AntigravityPlanlensImport(ctx context.Context, req AntigravityPlanlensImportRequest, commit bool) (*AntigravityPlanlensImportResult, error) {
	if err := validateAntImport(&req, commit); err != nil {
		return antImportFailure(err), nil
	}
	txrepo, ok := s.accountRepo.(antImportTransactionRepository)
	if !ok {
		return antImportResult("capability_missing"), nil
	}
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(req.DeadlineAt))
	defer cancel()
	if !commit {
		snap, err := antImportPrepare(ctx, s.accountRepo, s.groupRepo, s.proxyRepo, req)
		if err != nil {
			return antImportFailure(err), nil
		}
		result := antImportResult("ready")
		result.Platform = PlatformAntigravity
		result.Email = req.Email
		result.Revision = snap.revision
		if snap.account != nil {
			result.RemoteID = strconv.FormatInt(snap.account.ID, 10)
		}
		return result, nil
	}
	var committedID int64
	status := "imported"
	recoveredAuthError := false
	err := txrepo.WithAntigravityImportTransaction(ctx, func(accounts AccountRepository, groups GroupRepository, proxies ProxyRepository) (int64, error) {
		snap, err := antImportPrepare(ctx, accounts, groups, proxies, req)
		if err != nil {
			return 0, err
		}
		if snap.revision != req.Revision {
			return 0, errors.New("stale_revision")
		}
		if ctx.Err() != nil || req.DeadlineAt <= time.Now().UnixMilli() {
			return 0, errors.New("expired")
		}
		writer, ok := accounts.(antImportWriter)
		if !ok {
			return 0, errors.New("capability_missing")
		}
		generation := make([]byte, 16)
		if _, err := rand.Read(generation); err != nil {
			return 0, err
		}
		credentials := map[string]any{antigravityOAuthGeneration: hex.EncodeToString(generation), "_token_version": time.Now().UnixMilli(), "email": req.Email}
		for _, key := range []string{"access_token", "refresh_token", "project_id", "expires_at", "token_type"} {
			credentials[key] = req.OAuthAccount.Credentials[key]
		}
		if snap.account != nil {
			status = "updated"
			recoveredAuthError = AntigravityPlanlensRecoverableOAuthError(snap.account, time.Now())
			if err := writer.ApplyAntigravityImportCredentials(ctx, snap.account, credentials, req.DeadlineAt); err != nil {
				return 0, err
			}
			committedID = snap.account.ID
			return committedID, nil
		}
		if value, ok := req.OAuthAccount.Credentials["model_mapping"]; ok {
			credentials["model_mapping"] = value
		}
		cfg := snap.template
		account := &Account{Name: req.Email, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: credentials, Extra: map[string]any{}, Concurrency: cfg.Concurrency, Priority: cfg.Priority, RateMultiplier: cfg.RateMultiplier, GroupRateMultiplier: cfg.GroupRateMultiplier, LoadFactor: cfg.LoadFactor, AutoPauseOnExpired: cfg.AutoPauseOnExpired, Status: StatusActive, Schedulable: true}
		if snap.proxy != nil {
			if snap.proxy.ID == 0 {
				if err := proxies.Create(ctx, snap.proxy); err != nil {
					return 0, err
				}
			}
			id := snap.proxy.ID
			account.ProxyID = &id
		}
		if err := writer.CreateWithAccountGroups(ctx, account, []AccountGroup{{GroupID: cfg.GroupID, Priority: cfg.GroupPriority, AllowedModels: cfg.AllowedModels}}); err != nil {
			return 0, err
		}
		committedID = account.ID
		return account.ID, nil
	})
	if err != nil {
		return antImportFailure(err), nil
	}
	if recoveredAuthError && s.runtimeBlocker != nil {
		s.runtimeBlocker.ClearAccountSchedulingBlock(committedID)
	}
	result := antImportResult(status)
	result.Platform = PlatformAntigravity
	result.Email = req.Email
	result.RemoteID = strconv.FormatInt(committedID, 10)
	return result, nil
}

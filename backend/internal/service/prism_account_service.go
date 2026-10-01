package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const PrismSourceAccountKey = "prism_source_account_id"
const PrismProviderPreset = "prism_browser"

// prismWSModeExtraKey holds the account's WebSocket ingress mode. Prism has no
// upstream WebSocket, so WS clients are served by bridging each turn to HTTP.
const prismWSModeExtraKey = "openai_apikey_responses_websockets_v2_mode"

type PrismRuntimeConfig struct {
	URL           string
	ManagementKey string
}

type PrismTokenProvider interface {
	GetAccessToken(context.Context, *Account) (string, error)
}

type PrismAccountCreator interface {
	CreateAccount(context.Context, *CreateAccountInput) (*Account, error)
	ValidateAccountGroupBindings(context.Context, []int64) error
}

type PrismStatus struct {
	Phase           string   `json:"phase"`
	Ready           bool     `json:"ready"`
	Models          []string `json:"models"`
	Concurrency     int      `json:"concurrency"`
	ReadyWorkers    *int     `json:"ready_workers,omitempty"`
	BusyWorkers     *int     `json:"busy_workers,omitempty"`
	Queued          *int     `json:"queued,omitempty"`
	ErrorCode       string   `json:"error_code,omitempty"`
	LastHeartbeatAt string   `json:"last_heartbeat_at,omitempty"`
	SourceAccountID int64    `json:"source_account_id"`
	AccountID       int64    `json:"account_id"`
	Enabled         bool     `json:"enabled"`
}

type prismRuntimeStatus struct {
	Phase           string          `json:"phase"`
	Ready           bool            `json:"ready"`
	Models          []string        `json:"models"`
	Concurrency     *int            `json:"concurrency"`
	ReadyWorkers    *int            `json:"ready_workers,omitempty"`
	BusyWorkers     *int            `json:"busy_workers,omitempty"`
	Queued          *int            `json:"queued,omitempty"`
	ErrorCode       string          `json:"error_code"`
	LastHeartbeatAt json.RawMessage `json:"last_heartbeat_at"`
}

type prismSyncStamp struct {
	digest [32]byte
	at     time.Time
}

type PrismAccountService struct {
	repo    AccountRepository
	admin   PrismAccountCreator
	tokens  PrismTokenProvider
	cfg     PrismRuntimeConfig
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	create  sync.Mutex
	jobs    map[int64]bool
	pending map[int64]bool
	queue   []int64
	synced  map[int64]prismSyncStamp
	locks   sync.Map
	wg      sync.WaitGroup
	start   sync.Once
}

func NewPrismAccountService(repo AccountRepository, admin PrismAccountCreator, tokens PrismTokenProvider, cfg PrismRuntimeConfig) *PrismAccountService {
	ctx, cancel := context.WithCancel(context.Background())
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		cfg.URL = ""
	} else {
		cfg.URL = u.String()
	}
	return &PrismAccountService{repo: repo, admin: admin, tokens: tokens, cfg: cfg,
		ctx: ctx, cancel: cancel, jobs: make(map[int64]bool), pending: make(map[int64]bool), synced: make(map[int64]prismSyncStamp),
		client: &http.Client{Timeout: 270 * time.Second,
			Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func ProvidePrismAccountService(repo AccountRepository, groups GroupRepository, tokens *OpenAITokenProvider, cfg *config.Config) *PrismAccountService {
	runtimeConfig := PrismRuntimeConfig{URL: os.Getenv("PRISM_BROWSER_BASE_URL"), ManagementKey: os.Getenv("PRISM_MANAGEMENT_KEY")}
	if strings.EqualFold(os.Getenv("PRISM_BROWSER_ENABLED"), "false") {
		runtimeConfig.URL = ""
	}
	// API-key creation uses repository/group policy only; the full admin service
	// depends on the gateway that consumes this service.
	creator := &adminServiceImpl{accountRepo: repo, groupRepo: groups, cfg: cfg}
	s := NewPrismAccountService(repo, creator, tokens, runtimeConfig)
	startBackgroundService(cfg, s)
	return s
}

func (s *PrismAccountService) enabled() bool {
	return s != nil && s.cfg.URL != "" && s.cfg.ManagementKey != "" && s.tokens != nil
}

func (a *Account) IsManagedPrismAccount() bool {
	return a != nil && a.Platform == PlatformOpenAI && a.Type == AccountTypeAPIKey &&
		a.Extra["provider_preset"] == PrismProviderPreset && a.PrismSourceAccountID() > 0
}

func (a *Account) PrismSourceAccountID() int64 {
	if a == nil {
		return 0
	}
	switch v := a.Extra[PrismSourceAccountKey].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		if v > 0 && v == float64(int64(v)) {
			return int64(v)
		}
	case json.Number:
		id, _ := v.Int64()
		return id
	}
	return 0
}

func (s *PrismAccountService) source(ctx context.Context, id int64) (*Account, error) {
	a, err := s.repo.GetByID(ctx, id)
	if err != nil || a == nil || a.Platform != PlatformOpenAI || a.Type != AccountTypeOAuth || a.IsCredentialShadow() || a.IsOpenAIAgentIdentity() {
		return nil, infraerrors.BadRequest("PRISM_SOURCE_INVALID", "Prism requires an existing OpenAI OAuth account")
	}
	if !a.IsActive() || (a.ExpiresAt != nil && !time.Now().Before(*a.ExpiresAt)) {
		return nil, infraerrors.BadRequest("PRISM_SOURCE_DISABLED", "The source OAuth account is disabled or expired")
	}
	if !ObserverCanManageAccount(ctx, a) {
		return nil, ErrObserverScope
	}
	return a, nil
}

func (s *PrismAccountService) Create(ctx context.Context, sourceID int64, name string, groupIDs []int64) (*Account, *PrismStatus, error) {
	if !s.enabled() {
		return nil, nil, infraerrors.New(http.StatusServiceUnavailable, "PRISM_NOT_CONFIGURED", "The Prism browser service is not configured")
	}
	s.create.Lock()
	defer s.create.Unlock()
	source, err := s.source(ctx, sourceID)
	if err != nil {
		return nil, nil, err
	}
	if !ObserverCanManageAccount(ctx, source) {
		return nil, nil, ErrObserverScope
	}
	existing, err := s.repo.FindByExtraField(ctx, PrismSourceAccountKey, sourceID)
	if err != nil {
		return nil, nil, err
	}
	for i := range existing {
		if existing[i].IsManagedPrismAccount() {
			if !ObserverCanManageAccount(ctx, &existing[i]) {
				return nil, nil, ErrObserverScope
			}
			return &existing[i], s.storedStatus(&existing[i]), nil
		}
	}
	if groupIDs == nil {
		groupIDs = ObserverVisibleGroups(ctx, source.GroupIDs)
	}
	if err := s.admin.ValidateAccountGroupBindings(ctx, groupIDs); err != nil {
		return nil, nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		base := []rune(source.Name)
		if len(base) > 92 {
			base = base[:92]
		}
		name = string(base) + " (Prism)"
	}
	if len([]rune(name)) > maxAccountNameRunes {
		return nil, nil, infraerrors.BadRequest("PRISM_ACCOUNT_INVALID", "Account name exceeds 100 characters")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, nil, err
	}
	paused := false
	a, err := s.admin.CreateAccount(ctx, &CreateAccountInput{
		Name: name, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Priority: source.Priority, GroupIDs: groupIDs, SkipDefaultGroupBind: true,
		Schedulable: &paused, prismManaged: true,
		Credentials: map[string]any{"api_key": "prism_" + hex.EncodeToString(key),
			"base_url":      s.cfg.URL + "/accounts/" + strconv.FormatInt(sourceID, 10) + "/v1",
			"model_mapping": map[string]string{}, "model_mapping_mode": "whitelist",
			"openai_capabilities": []string{"chat_completions"}},
		Extra: map[string]any{"provider_preset": PrismProviderPreset, PrismSourceAccountKey: sourceID,
			"prism_phase": "provisioning", "prism_auto_enable_pending": true,
			"openai_responses_mode": "force_responses", "openai_passthrough": false,
			prismWSModeExtraKey: OpenAIWSIngressModeHTTPBridge},
	})
	if err != nil {
		// The unique index resolves retries across application instances.
		if found, findErr := s.repo.FindByExtraField(ctx, PrismSourceAccountKey, sourceID); findErr == nil {
			for i := range found {
				if found[i].IsManagedPrismAccount() && ObserverCanManageAccount(ctx, &found[i]) {
					return &found[i], s.storedStatus(&found[i]), nil
				}
			}
		}
		return nil, nil, err
	}
	s.schedule(a.ID, false)
	return a, s.storedStatus(a), nil
}

func (s *PrismAccountService) storedStatus(a *Account) *PrismStatus {
	status := &PrismStatus{Phase: "provisioning", Models: []string{}, AccountID: a.ID,
		SourceAccountID: a.PrismSourceAccountID(), Enabled: s.enabled(), Concurrency: 1}
	if a.Concurrency >= 1 && a.Concurrency <= 4 {
		status.Concurrency = a.Concurrency
	}
	for _, metric := range []struct {
		key    string
		target **int
	}{
		{"prism_ready_workers", &status.ReadyWorkers},
		{"prism_busy_workers", &status.BusyWorkers},
		{"prism_queued", &status.Queued},
	} {
		if raw, err := json.Marshal(a.Extra[metric.key]); err == nil {
			_ = json.Unmarshal(raw, metric.target)
		}
		if *metric.target != nil && (**metric.target < 0 || metric.key != "prism_queued" && **metric.target > status.Concurrency) {
			*metric.target = nil
		}
	}
	if v, ok := a.Extra["prism_phase"].(string); ok && v != "" {
		status.Phase = v
	}
	status.Ready = status.Phase == "ready"
	status.ErrorCode, _ = a.Extra["prism_error_code"].(string)
	status.LastHeartbeatAt, _ = a.Extra["prism_last_heartbeat_at"].(string)
	if raw, err := json.Marshal(a.Extra["prism_models"]); err == nil {
		_ = json.Unmarshal(raw, &status.Models)
	}
	if status.Models == nil {
		status.Models = []string{}
	}
	if !status.Enabled {
		status.Phase, status.Ready, status.ErrorCode = "disabled", false, "PRISM_NOT_CONFIGURED"
	}
	return status
}

func (s *PrismAccountService) managed(ctx context.Context, id int64) (*Account, error) {
	a, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !a.IsManagedPrismAccount() {
		return nil, infraerrors.BadRequest("PRISM_ACCOUNT_INVALID", "This is not a managed Prism account")
	}
	if !ObserverCanManageAccount(ctx, a) {
		return nil, ErrObserverScope
	}
	return a, nil
}

func (s *PrismAccountService) Status(ctx context.Context, id int64) (*PrismStatus, error) {
	a, err := s.managed(ctx, id)
	if err != nil {
		return nil, err
	}
	status := s.storedStatus(a)
	if !s.enabled() {
		return status, nil
	}
	if !a.IsActive() {
		status.Phase, status.Ready, status.ErrorCode = "disabled", false, "PRISM_ACCOUNT_DISABLED"
		return status, nil
	}
	if _, err := s.source(ctx, a.PrismSourceAccountID()); err != nil {
		if errors.Is(err, ErrObserverScope) {
			return nil, err
		}
		status.Phase, status.Ready, status.ErrorCode = "disabled", false, "PRISM_SOURCE_DISABLED"
		return status, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var runtime prismRuntimeStatus
	if s.request(ctx, a.PrismSourceAccountID(), http.MethodGet, "status", nil, &runtime) == nil {
		s.applyRuntimeStatus(status, runtime)
	} else {
		status.Phase, status.Ready, status.ErrorCode = "error", false, "PRISM_UPSTREAM_UNAVAILABLE"
	}
	return status, nil
}

func (s *PrismAccountService) Reconnect(ctx context.Context, id int64) (*PrismStatus, error) {
	if !s.enabled() {
		return nil, infraerrors.New(http.StatusServiceUnavailable, "PRISM_NOT_CONFIGURED", "The Prism browser service is not configured")
	}
	a, err := s.managed(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.source(ctx, a.PrismSourceAccountID()); err != nil {
		return nil, err
	}
	if !a.IsActive() {
		return nil, infraerrors.BadRequest("PRISM_ACCOUNT_DISABLED", "Enable the Prism account before reconnecting")
	}
	if err := s.repo.UpdateExtra(ctx, id, map[string]any{"prism_phase": "provisioning", "prism_error_code": "",
		"prism_auto_enable_pending": a.Schedulable || a.Extra["prism_auto_enable_pending"] == true}); err != nil {
		return nil, err
	}
	if err := s.repo.SetSchedulable(ctx, id, false); err != nil {
		return nil, err
	}
	s.schedule(id, true)
	return &PrismStatus{Phase: "provisioning", Models: []string{}, Enabled: true, AccountID: id,
		SourceAccountID: a.PrismSourceAccountID(), Concurrency: s.storedStatus(a).Concurrency}, nil
}

func (s *PrismAccountService) request(ctx context.Context, sourceID int64, method, action string, body any, result any) error {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method,
		s.cfg.URL+"/internal/accounts/"+strconv.FormatInt(sourceID, 10)+"/"+action, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.ManagementKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return infraerrors.New(http.StatusServiceUnavailable, "PRISM_UPSTREAM_UNAVAILABLE", "The Prism browser service is unavailable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return infraerrors.New(http.StatusServiceUnavailable, "PRISM_UPSTREAM_UNAVAILABLE", "The Prism browser service rejected the request")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &failure) == nil && prismErrorCode.MatchString(failure.Error.Code) {
			return infraerrors.New(http.StatusServiceUnavailable, failure.Error.Code, "The Prism browser service rejected the request")
		}
		return infraerrors.New(http.StatusServiceUnavailable, "PRISM_UPSTREAM_UNAVAILABLE", "The Prism browser service rejected the request")
	}
	if result != nil && json.Unmarshal(data, result) != nil {
		return infraerrors.New(http.StatusServiceUnavailable, "PRISM_UPSTREAM_UNAVAILABLE", "Invalid response from the Prism browser service")
	}
	return nil
}

func prismTokenClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	_ = json.Unmarshal(data, &claims)
	return claims
}

func (s *PrismAccountService) syncSession(ctx context.Context, a, source *Account, force bool) error {
	if !s.enabled() {
		return infraerrors.New(http.StatusServiceUnavailable, "PRISM_NOT_CONFIGURED", "The managed Prism endpoint is unavailable")
	}
	lock, _ := s.locks.LoadOrStore(source.ID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	fresh, loadErr := s.repo.GetByID(ctx, a.ID)
	if loadErr != nil || !fresh.IsManagedPrismAccount() || !fresh.IsActive() || fresh.PrismSourceAccountID() != source.ID {
		return infraerrors.BadRequest("PRISM_ACCOUNT_DISABLED", "The Prism account is disabled or deleted")
	}
	a = fresh
	var err error
	source, err = s.source(ctx, source.ID)
	if err != nil {
		return err
	}
	if a.GetOpenAIBaseURL() != s.cfg.URL+"/accounts/"+strconv.FormatInt(source.ID, 10)+"/v1" {
		return infraerrors.New(http.StatusServiceUnavailable, "PRISM_NOT_CONFIGURED", "The managed Prism endpoint is unavailable")
	}
	// Read the refresh owner's token under the same lock as its runtime upload.
	token, err := s.tokens.GetAccessToken(ctx, source)
	if err != nil {
		return infraerrors.New(http.StatusServiceUnavailable, "PRISM_SOURCE_DISABLED", "Unable to refresh the source OAuth account")
	}
	key := a.GetOpenAIProtocolAPIKey()
	digest := sha256.Sum256([]byte(token + "\x00" + key))
	s.mu.Lock()
	stamp := s.synced[source.ID]
	s.mu.Unlock()
	if !force && stamp.digest == digest {
		return nil
	}
	claims := prismTokenClaims(token)
	profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	email := source.GetCredential("email")
	if email == "" {
		email, _ = profile["email"].(string)
	}
	if email == "" {
		email, _ = claims["email"].(string)
	}
	userID := ""
	if email == "" {
		userID, _ = auth["chatgpt_user_id"].(string)
		if userID == "" {
			userID, _ = claims["sub"].(string)
		}
	}
	if email == "" && userID == "" {
		return infraerrors.BadRequest("PRISM_SOURCE_INVALID", "The OAuth account has no verifiable user identity")
	}
	expiresAt, _ := claims["exp"].(float64)
	if expiresAt == 0 {
		if expiry := source.GetCredentialAsTime("expires_at"); expiry != nil {
			expiresAt = float64(expiry.Unix())
		}
	}
	if expiresAt <= float64(time.Now().Unix()) {
		return infraerrors.BadRequest("PRISM_SOURCE_DISABLED", "The source access token has expired")
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	payload := map[string]any{"access_token": token, "api_key": key, "expires_at": int64(expiresAt)}
	if email != "" {
		payload["expected_email"] = email
	} else {
		payload["expected_user_id"] = userID
	}
	if err := s.request(ctx, source.ID, http.MethodPut, "session", payload, nil); err != nil {
		return err
	}
	s.mu.Lock()
	s.synced[source.ID] = prismSyncStamp{digest: digest, at: time.Now()}
	s.mu.Unlock()
	return nil
}

func (s *PrismAccountService) EnsureSession(ctx context.Context, a *Account) error {
	if !a.IsManagedPrismAccount() {
		return nil
	}
	fresh, err := s.repo.GetByID(ctx, a.ID)
	if err != nil || !fresh.IsManagedPrismAccount() || !fresh.IsActive() || !fresh.Schedulable {
		return infraerrors.New(http.StatusServiceUnavailable, "PRISM_ACCOUNT_NOT_READY", "The Prism account is disabled or reconnecting")
	}
	a = fresh
	source, err := s.source(ctx, a.PrismSourceAccountID())
	if err == nil {
		err = s.syncSession(ctx, a, source, false)
	}
	if err == nil {
		var status prismRuntimeStatus
		statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = s.request(statusCtx, source.ID, http.MethodGet, "status", nil, &status)
		cancel()
		_, capacityValid := prismRuntimeCapacity(status)
		if err == nil && (!status.Ready || !capacityValid) {
			s.schedule(a.ID, false)
			err = infraerrors.New(http.StatusServiceUnavailable, "PRISM_ACCOUNT_NOT_READY", "The Prism account is reconnecting")
		}
	}
	if err != nil {
		s.mu.Lock()
		delete(s.synced, a.PrismSourceAccountID())
		s.mu.Unlock()
		// A client that went away mid-check says nothing about the session.
		if ctx.Err() == nil {
			s.schedule(a.ID, false)
			_ = s.repo.SetTempUnschedulable(ctx, a.ID, time.Now().Add(90*time.Second), "prism: session unavailable")
		}
	}
	return err
}

var prismModelID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
var prismErrorCode = regexp.MustCompile(`^[a-z][a-z0-9_]{1,80}$`)

func prismRuntimeCapacity(runtime prismRuntimeStatus) (int, bool) {
	// Missing capacity is the legacy single-worker protocol. Invalid reports
	// cannot increase scheduler concurrency or mark the account ready.
	capacity := 1
	if runtime.Concurrency != nil {
		capacity = *runtime.Concurrency
	}
	if capacity < 0 || capacity > 4 || capacity == 0 && runtime.Ready {
		return 1, false
	}
	if runtime.ReadyWorkers != nil && *runtime.ReadyWorkers != capacity {
		return 1, false
	}
	if runtime.BusyWorkers != nil && (*runtime.BusyWorkers < 0 || *runtime.BusyWorkers > capacity) {
		return 1, false
	}
	if runtime.Queued != nil && *runtime.Queued < 0 {
		return 1, false
	}
	return capacity, true
}

func (s *PrismAccountService) applyRuntimeStatus(status *PrismStatus, runtime prismRuntimeStatus) {
	status.Phase, status.Ready, status.ErrorCode = runtime.Phase, runtime.Ready, runtime.ErrorCode
	capacity, capacityValid := prismRuntimeCapacity(runtime)
	status.Concurrency = capacity
	status.ReadyWorkers, status.BusyWorkers, status.Queued = runtime.ReadyWorkers, runtime.BusyWorkers, runtime.Queued
	switch status.Phase {
	case "authenticating":
		status.Phase = "provisioning"
	case "authenticated":
		status.Phase = "session_ready"
	case "initializing", "verifying_model":
		status.Phase = "sandbox_syncing"
	case "ready":
	default:
		status.Phase = "error"
		status.Ready = false
	}
	status.Models = []string{}
	for _, model := range runtime.Models {
		if prismModelID.MatchString(model) && !slices.Contains(status.Models, model) {
			status.Models = append(status.Models, model)
		}
	}
	if len(status.Models) == 0 && status.Ready {
		status.Ready, status.Phase, status.ErrorCode = false, "error", "prism_models_unavailable"
	}
	if !capacityValid {
		status.Ready, status.Phase, status.ErrorCode = false, "error", "prism_invalid_capacity"
		status.ReadyWorkers, status.BusyWorkers, status.Queued = nil, nil, nil
	}
	var seconds float64
	if json.Unmarshal(runtime.LastHeartbeatAt, &seconds) == nil && seconds > 0 {
		status.LastHeartbeatAt = time.Unix(int64(seconds), 0).UTC().Format(time.RFC3339)
	} else {
		_ = json.Unmarshal(runtime.LastHeartbeatAt, &status.LastHeartbeatAt)
	}
}

func (s *PrismAccountService) syncAccount(ctx context.Context, id int64, retryProbe bool) {
	a, err := s.repo.GetByID(ctx, id)
	if err != nil || !a.IsManagedPrismAccount() {
		return
	}
	// Accounts created before WebSocket clients were bridged to HTTP carry "off".
	// The value is managed (admins cannot edit it), so the worker owns the repair.
	if a.Extra[prismWSModeExtraKey] != OpenAIWSIngressModeHTTPBridge {
		if s.repo.UpdateExtra(ctx, id, map[string]any{prismWSModeExtraKey: OpenAIWSIngressModeHTTPBridge}) == nil {
			a.Extra[prismWSModeExtraKey] = OpenAIWSIngressModeHTTPBridge
		}
	}
	status := s.storedStatus(a)
	source, err := s.source(ctx, a.PrismSourceAccountID())
	if err == nil && a.IsActive() {
		err = s.syncSession(ctx, a, source, retryProbe)
	} else {
		_ = s.request(ctx, a.PrismSourceAccountID(), http.MethodDelete, "session", nil, nil)
		if err == nil {
			err = infraerrors.BadRequest("PRISM_ACCOUNT_DISABLED", "The Prism account is disabled")
		}
	}
	if err == nil {
		var runtime prismRuntimeStatus
		err = s.request(ctx, source.ID, http.MethodPost, "bootstrap", map[string]any{"retry_probe": retryProbe}, &runtime)
		if err == nil {
			s.applyRuntimeStatus(status, runtime)
		}
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		s.mu.Lock()
		delete(s.synced, a.PrismSourceAccountID())
		s.mu.Unlock()
		status.Phase, status.Ready, status.ErrorCode = "error", false, infraerrors.Reason(err)
		if status.ErrorCode == "" {
			status.ErrorCode = "PRISM_UPSTREAM_UNAVAILABLE"
		}
		if status.ErrorCode == "PRISM_SOURCE_DISABLED" || status.ErrorCode == "PRISM_SOURCE_INVALID" || status.ErrorCode == "PRISM_ACCOUNT_DISABLED" {
			status.Phase = "disabled"
		}
	}
	updates := map[string]any{"prism_phase": status.Phase, "prism_error_code": status.ErrorCode,
		"prism_models": status.Models, "prism_last_heartbeat_at": status.LastHeartbeatAt,
		"prism_concurrency": status.Concurrency, "prism_ready_workers": status.ReadyWorkers,
		"prism_busy_workers": status.BusyWorkers, "prism_queued": status.Queued}
	if status.Ready {
		fresh, loadErr := s.repo.GetByID(ctx, id)
		if loadErr != nil || !fresh.IsManagedPrismAccount() || !fresh.IsActive() {
			_ = s.request(ctx, a.PrismSourceAccountID(), http.MethodDelete, "session", nil, nil)
			return
		}
		if _, sourceErr := s.source(ctx, fresh.PrismSourceAccountID()); sourceErr != nil {
			_ = s.request(ctx, fresh.PrismSourceAccountID(), http.MethodDelete, "session", nil, nil)
			return
		}
		mapping := make(map[string]any, len(status.Models))
		for _, model := range status.Models {
			mapping[model] = model
		}
		current, _ := json.Marshal(fresh.Credentials["model_mapping"])
		desired, _ := json.Marshal(mapping)
		var updateErr error
		if !bytes.Equal(current, desired) || fresh.Concurrency != status.Concurrency {
			_, updateErr = s.repo.BulkUpdate(ctx, []int64{id}, AccountBulkUpdate{
				Credentials: map[string]any{"model_mapping": mapping}, Concurrency: &status.Concurrency})
		}
		if updateErr != nil {
			status.Ready = false
			updates["prism_phase"], updates["prism_error_code"] = "error", "prism_account_update_failed"
		} else {
			fresh, loadErr := s.repo.GetByID(ctx, id)
			if loadErr == nil && fresh.IsActive() && fresh.Extra["prism_auto_enable_pending"] == true {
				if s.repo.SetSchedulable(ctx, id, true) == nil {
					updates["prism_auto_enable_pending"] = false
				}
			}
			if fresh != nil && strings.HasPrefix(fresh.TempUnschedulableReason, "prism:") {
				_ = s.repo.ClearTempUnschedulable(ctx, id)
			}
		}
	}
	if !status.Ready {
		_ = s.repo.SetTempUnschedulable(ctx, id, time.Now().Add(90*time.Second), "prism: session unavailable")
	}
	_ = s.repo.UpdateExtra(ctx, id, updates)
}

func (s *PrismAccountService) schedule(id int64, retryProbe bool) {
	if !s.enabled() {
		return
	}
	s.mu.Lock()
	if s.ctx.Err() != nil || s.jobs[id] && !retryProbe {
		s.mu.Unlock()
		return
	}
	if prior, exists := s.pending[id]; exists {
		s.pending[id] = prior || retryProbe
	} else {
		s.pending[id] = retryProbe
		s.queue = append(s.queue, id)
	}
	s.dispatchLocked()
	s.mu.Unlock()
}

func (s *PrismAccountService) dispatchLocked() {
	if s.ctx.Err() != nil {
		return
	}
	for i := 0; i < len(s.queue) && len(s.jobs) < 4; {
		id := s.queue[i]
		if s.jobs[id] {
			i++
			continue
		}
		retryProbe := s.pending[id]
		delete(s.pending, id)
		s.queue = append(s.queue[:i], s.queue[i+1:]...)
		s.launchLocked(id, retryProbe)
	}
}

func (s *PrismAccountService) launchLocked(id int64, retryProbe bool) {
	s.jobs[id] = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { s.mu.Lock(); delete(s.jobs, id); s.dispatchLocked(); s.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(s.ctx, 250*time.Second)
		defer cancel()
		s.syncAccount(ctx, id, retryProbe)
	}()
}

func (s *PrismAccountService) Start() {
	if !s.enabled() {
		return
	}
	s.start.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				accounts, err := s.repo.FindByExtraField(s.ctx, "provider_preset", PrismProviderPreset)
				if err == nil {
					s.cleanupDeleted(accounts)
					for _, account := range accounts {
						if account.IsManagedPrismAccount() {
							s.schedule(account.ID, false)
						}
					}
				}
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}

func (s *PrismAccountService) cleanupDeleted(accounts []Account) {
	present := make(map[int64]bool, len(accounts))
	for _, a := range accounts {
		if a.IsManagedPrismAccount() {
			present[a.PrismSourceAccountID()] = true
		}
	}
	s.mu.Lock()
	var stale []int64
	for source := range s.synced {
		if !present[source] {
			stale = append(stale, source)
		}
	}
	s.mu.Unlock()
	for _, source := range stale {
		lock, _ := s.locks.LoadOrStore(source, &sync.Mutex{})
		lock.(*sync.Mutex).Lock()
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		current, err := s.repo.FindByExtraField(ctx, PrismSourceAccountKey, source)
		if err == nil && !slices.ContainsFunc(current, func(a Account) bool { return a.IsManagedPrismAccount() }) {
			if s.request(ctx, source, http.MethodDelete, "session", nil, nil) == nil {
				s.mu.Lock()
				delete(s.synced, source)
				s.mu.Unlock()
			}
		}
		cancel()
		lock.(*sync.Mutex).Unlock()
	}
}

func (s *PrismAccountService) RevokeDeleted(ctx context.Context, account *Account) {
	if !s.enabled() || account == nil {
		return
	}
	source := account.PrismSourceAccountID()
	if !account.IsManagedPrismAccount() {
		if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
			return
		}
		linked, err := s.repo.FindByExtraField(ctx, PrismSourceAccountKey, account.ID)
		if err != nil || !slices.ContainsFunc(linked, func(a Account) bool { return a.IsManagedPrismAccount() }) {
			return
		}
		source = account.ID
	}
	lock, _ := s.locks.LoadOrStore(source, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	s.mu.Lock()
	s.synced[source] = prismSyncStamp{}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = s.request(ctx, source, http.MethodDelete, "session", nil, nil)
}

func (s *PrismAccountService) Stop() {
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
	s.client.CloseIdleConnections()
}

func preservePrismExtra(account *Account, extra map[string]any) map[string]any {
	if !account.IsManagedPrismAccount() {
		return extra
	}
	extra = maps.Clone(extra)
	if extra == nil {
		extra = make(map[string]any)
	}
	for key, value := range account.Extra {
		if strings.HasPrefix(key, "prism_") || key == "provider_preset" || key == "openai_responses_mode" || key == "openai_passthrough" || strings.Contains(key, "websockets") {
			extra[key] = value
		}
	}
	return extra
}

func validatePrismAccountUpdate(account *Account, input *UpdateAccountInput) error {
	if !account.IsManagedPrismAccount() {
		if hasPrismManagedExtra(input.Extra) {
			return infraerrors.BadRequest("PRISM_MANAGED_FIELDS_IMMUTABLE", "Use the Prism account creation action")
		}
		return nil
	}
	if input.Type != "" && input.Type != AccountTypeAPIKey || input.ProxyID != nil && *input.ProxyID != 0 || input.Concurrency != nil && *input.Concurrency != account.Concurrency || input.ProbeEnabled != nil && *input.ProbeEnabled || input.RateSyncEnabled != nil && *input.RateSyncEnabled {
		return infraerrors.BadRequest("PRISM_MANAGED_FIELDS_IMMUTABLE", "Managed Prism connection settings cannot be changed")
	}
	if value, ok := input.Extra[PrismSourceAccountKey]; ok {
		candidate := &Account{Extra: map[string]any{PrismSourceAccountKey: value}}
		if candidate.PrismSourceAccountID() != account.PrismSourceAccountID() {
			return infraerrors.BadRequest("PRISM_MANAGED_FIELDS_IMMUTABLE", "The source OAuth account cannot be changed")
		}
	}
	if preset, ok := input.Extra["provider_preset"]; ok && preset != PrismProviderPreset {
		return infraerrors.BadRequest("PRISM_MANAGED_FIELDS_IMMUTABLE", "The managed Prism provider cannot be changed")
	}
	for key, value := range input.Credentials {
		old, oldErr := json.Marshal(account.Credentials[key])
		incoming, incomingErr := json.Marshal(value)
		if oldErr != nil || incomingErr != nil || !bytes.Equal(old, incoming) {
			return infraerrors.BadRequest("PRISM_MANAGED_FIELDS_IMMUTABLE", "Managed Prism credentials cannot be changed")
		}
	}
	return nil
}

func hasPrismManagedExtra(extra map[string]any) bool {
	for key, value := range extra {
		if strings.HasPrefix(key, "prism_") || key == "provider_preset" && value == PrismProviderPreset {
			return true
		}
	}
	return false
}

func prismConnectionExtraChanged(account *Account, extra map[string]any) bool {
	for key, value := range extra {
		if key == "openai_responses_mode" || key == "openai_passthrough" || strings.Contains(key, "websockets") {
			old, _ := json.Marshal(account.Extra[key])
			incoming, _ := json.Marshal(value)
			if !bytes.Equal(old, incoming) {
				return true
			}
		}
	}
	return false
}

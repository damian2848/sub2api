package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type prismTestRepository struct {
	AccountRepository
	mu               sync.Mutex
	items            map[int64]*Account
	created          []*Account
	nextID           int64
	bulkCalls        int
	extraCalls       int
	schedulableCalls int
}

func prismTestClone(a *Account) *Account {
	if a == nil {
		return nil
	}
	copy := *a
	copy.GroupIDs = slices.Clone(a.GroupIDs)
	copy.Credentials, copy.Extra = nil, nil
	for _, field := range []struct{ in, out *map[string]any }{{&a.Credentials, &copy.Credentials}, {&a.Extra, &copy.Extra}} {
		data, _ := json.Marshal(*field.in)
		_ = json.Unmarshal(data, field.out)
	}
	return &copy
}

func newPrismTestRepository(accounts ...*Account) *prismTestRepository {
	r := &prismTestRepository{items: map[int64]*Account{}, nextID: 100}
	for _, account := range accounts {
		r.items[account.ID] = prismTestClone(account)
	}
	return r
}

func (r *prismTestRepository) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.items[id]; a != nil {
		return prismTestClone(a), nil
	}
	return nil, ErrAccountNotFound
}

func (r *prismTestRepository) GetByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	var accounts []*Account
	for _, id := range ids {
		if a, err := r.GetByID(ctx, id); err == nil {
			accounts = append(accounts, a)
		}
	}
	return accounts, nil
}

func (r *prismTestRepository) FindByExtraField(_ context.Context, key string, value any) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var accounts []Account
	for _, account := range r.items {
		if fmt.Sprint(account.Extra[key]) == fmt.Sprint(value) {
			accounts = append(accounts, *prismTestClone(account))
		}
	}
	return accounts, nil
}

func (r *prismTestRepository) Create(_ context.Context, a *Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	a.ID = r.nextID
	r.items[a.ID] = prismTestClone(a)
	r.created = append(r.created, prismTestClone(a))
	return nil
}

func (r *prismTestRepository) Update(_ context.Context, a *Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[a.ID] = prismTestClone(a)
	return nil
}

func (r *prismTestRepository) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extraCalls++
	a := r.items[id]
	if a == nil {
		return ErrAccountNotFound
	}
	if a.Extra == nil {
		a.Extra = map[string]any{}
	}
	for key, value := range updates {
		a.Extra[key] = value
	}
	r.items[id] = prismTestClone(a)
	return nil
}

func (r *prismTestRepository) SetSchedulable(_ context.Context, id int64, value bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schedulableCalls++
	if r.items[id] == nil {
		return ErrAccountNotFound
	}
	r.items[id].Schedulable = value
	return nil
}

func (r *prismTestRepository) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items[id] == nil {
		return ErrAccountNotFound
	}
	r.items[id].TempUnschedulableUntil, r.items[id].TempUnschedulableReason = &until, reason
	return nil
}

func (r *prismTestRepository) ClearTempUnschedulable(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items[id] == nil {
		return ErrAccountNotFound
	}
	r.items[id].TempUnschedulableUntil, r.items[id].TempUnschedulableReason = nil, ""
	return nil
}

func (r *prismTestRepository) BulkUpdate(_ context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bulkCalls++
	var updated int64
	for _, id := range ids {
		a := r.items[id]
		if a == nil {
			continue
		}
		for key, value := range updates.Credentials {
			a.Credentials[key] = value
		}
		for key, value := range updates.Extra {
			a.Extra[key] = value
		}
		if updates.Schedulable != nil {
			a.Schedulable = *updates.Schedulable
		}
		r.items[id] = prismTestClone(a)
		updated++
	}
	return updated, nil
}

type prismTestAdmin struct {
	AdminService
	repo *prismTestRepository
}

func (a *prismTestAdmin) ValidateAccountGroupBindings(context.Context, []int64) error { return nil }

func (a *prismTestAdmin) CreateAccount(ctx context.Context, input *CreateAccountInput) (*Account, error) {
	account := &Account{Name: input.Name, Platform: input.Platform, Type: input.Type, Status: StatusActive,
		Schedulable: true, Credentials: input.Credentials, Extra: input.Extra, Concurrency: input.Concurrency,
		Priority: input.Priority, GroupIDs: input.GroupIDs}
	if input.Schedulable != nil {
		account.Schedulable = *input.Schedulable
	}
	return account, a.repo.Create(ctx, account)
}

func (a *prismTestAdmin) UpdateAccount(ctx context.Context, id int64, input *UpdateAccountInput) (*Account, error) {
	account, err := a.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	for key, value := range input.Credentials {
		account.Credentials[key] = value
	}
	return account, a.repo.Update(ctx, account)
}

type prismTestTokens struct {
	mu     sync.Mutex
	token  string
	err    error
	owners []int64
}

type prismTestTokenFunc func(context.Context, *Account) (string, error)

func (f prismTestTokenFunc) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	return f(ctx, account)
}

func (p *prismTestTokens) GetAccessToken(_ context.Context, account *Account) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.owners = append(p.owners, account.ID)
	return p.token, p.err
}

func prismTestJWT(label string) string {
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "test_label": label,
		"https://api.openai.com/profile": map[string]any{"email": "prism-owner@example.test"}})
	return "test." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}

func prismTestSource() *Account {
	return &Account{ID: 32, Name: "OAuth source", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, GroupIDs: []int64{10},
		Credentials: map[string]any{"access_token": "old-source-access", "refresh_token": "source-refresh-only",
			"email": "prism-owner@example.test", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
}

func prismTestManaged(endpoint string) *Account {
	return &Account{ID: 101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
		GroupIDs: []int64{10}, Concurrency: 1,
		Credentials: map[string]any{"api_key": strings.Repeat("k", 64), "base_url": endpoint + "/accounts/32/v1"},
		Extra: map[string]any{"provider_preset": PrismProviderPreset, PrismSourceAccountKey: int64(32),
			"prism_phase": "provisioning", "prism_auto_enable_pending": true}}
}

func prismTestService(t *testing.T, endpoint string, repo *prismTestRepository, tokens *prismTestTokens) *PrismAccountService {
	t.Helper()
	svc := NewPrismAccountService(repo, &prismTestAdmin{repo: repo}, tokens,
		PrismRuntimeConfig{URL: endpoint, ManagementKey: "management-test-secret"})
	t.Cleanup(svc.Stop)
	return svc
}

func prismTestWaitJobs(svc *PrismAccountService) { svc.wg.Wait() }

func TestPrismCreatePendingIdempotentAndKeepsOAuthTokenOwner(t *testing.T) {
	gate := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate) }) }
	var gotSession map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer management-test-secret", r.Header.Get("Authorization"))
		if r.Method == http.MethodPut {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotSession))
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"phase":"ready","ready":true,"models":["gpt-6.1-sol"],"last_heartbeat_at":1790841600}`))
	}))
	t.Cleanup(server.Close)
	source := prismTestSource()
	repo := newPrismTestRepository(source)
	tokens := &prismTestTokens{token: prismTestJWT("latest")}
	svc := prismTestService(t, server.URL, repo, tokens)
	t.Cleanup(unblock)
	account, status, err := svc.Create(context.Background(), 32, "", nil)
	require.NoError(t, err)
	require.False(t, account.Schedulable, "new account must be persisted paused before browser work starts")
	require.Equal(t, "provisioning", status.Phase)
	require.False(t, status.Ready)
	require.Equal(t, []int64{10}, account.GroupIDs)
	require.Equal(t, 1, account.Concurrency)
	require.NotContains(t, account.Credentials, "refresh_token")
	require.NotContains(t, account.Credentials, "access_token")
	require.Equal(t, "http_bridge", account.Extra["openai_apikey_responses_websockets_v2_mode"],
		"WebSocket clients are served by bridging each turn to HTTP")
	require.Equal(t, "force_responses", account.Extra["openai_responses_mode"])
	require.Equal(t, false, account.Extra["openai_passthrough"])
	second, _, err := svc.Create(context.Background(), 32, "ignored duplicate name", nil)
	require.NoError(t, err)
	require.Equal(t, account.ID, second.ID)
	repo.mu.Lock()
	require.Len(t, repo.created, 1)
	repo.mu.Unlock()
	unblock()
	prismTestWaitJobs(svc)
	require.Equal(t, tokens.token, gotSession["access_token"])
	require.Equal(t, "prism-owner@example.test", gotSession["expected_email"])
	require.NotContains(t, gotSession, "refresh_token")
	require.NotContains(t, gotSession, "id_token")
	fresh, err := repo.GetByID(context.Background(), account.ID)
	require.NoError(t, err)
	require.True(t, fresh.Schedulable)
	require.Equal(t, "ready", fresh.Extra["prism_phase"])
	require.Equal(t, false, fresh.Extra["prism_auto_enable_pending"])
	require.Contains(t, fresh.Credentials["model_mapping"], "gpt-6.1-sol")
	sourceAfter, err := repo.GetByID(context.Background(), 32)
	require.NoError(t, err)
	require.Equal(t, source.Credentials, sourceAfter.Credentials)
	require.False(t, sourceAfter.Schedulable)
	tokens.mu.Lock()
	require.Equal(t, []int64{32}, tokens.owners, "only the original OAuth account may be refreshed")
	tokens.mu.Unlock()
}

func TestPrismSourceValidationRejectsDisabledShadowAndUnauthorizedAccounts(t *testing.T) {
	parent := int64(31)
	for _, test := range []struct {
		name string
		edit func(*Account)
		ctx  context.Context
	}{
		{name: "inactive", edit: func(a *Account) { a.Status = "inactive" }},
		{name: "shadow", edit: func(a *Account) { a.ParentAccountID = &parent }},
		{name: "api_key", edit: func(a *Account) { a.Type = AccountTypeAPIKey }},
		{name: "other_platform", edit: func(a *Account) { a.Platform = PlatformAnthropic }},
		{name: "expired", edit: func(a *Account) { expiry := time.Now().Add(-time.Hour); a.ExpiresAt = &expiry }},
		{name: "outside_observer_scope", ctx: WithObserverScope(context.Background(), []int64{20})},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := prismTestSource()
			if test.edit != nil {
				test.edit(source)
			}
			repo := newPrismTestRepository(source)
			svc := prismTestService(t, "http://127.0.0.1:1", repo, &prismTestTokens{token: prismTestJWT("latest")})
			ctx := test.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			_, _, err := svc.Create(ctx, 32, "", nil)
			require.Error(t, err)
			require.Empty(t, repo.created)
		})
	}
}

func TestPrismBootstrapFailureDoesNotEnableAndExplicitReconnectAuthorizesProbeRetry(t *testing.T) {
	var mu sync.Mutex
	var operations []string
	var retryProbes []bool
	var firstFailure atomic.Bool
	firstFailure.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		operations = append(operations, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/bootstrap") {
			var body struct {
				RetryProbe bool `json:"retry_probe"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			retryProbes = append(retryProbes, body.RetryProbe)
			if firstFailure.Swap(false) {
				http.Error(w, "private browser failure", 503)
				return
			}
			_, _ = w.Write([]byte(`{"phase":"ready","ready":true,"models":["gpt-6.1-sol"]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	repo := newPrismTestRepository(prismTestSource(), prismTestManaged(server.URL))
	svc := prismTestService(t, server.URL, repo, &prismTestTokens{token: prismTestJWT("latest")})
	svc.syncAccount(context.Background(), 101, false)
	failed, _ := repo.GetByID(context.Background(), 101)
	require.False(t, failed.Schedulable)
	require.Equal(t, "error", failed.Extra["prism_phase"])
	require.NotContains(t, failed.Extra["prism_error_code"], "private")
	require.Equal(t, []string{"PUT /internal/accounts/32/session", "POST /internal/accounts/32/bootstrap"}, operations)
	status, err := svc.Reconnect(context.Background(), 101)
	require.NoError(t, err)
	require.False(t, status.Ready)
	prismTestWaitJobs(svc)
	ready, _ := repo.GetByID(context.Background(), 101)
	require.True(t, ready.Schedulable)
	require.Nil(t, ready.TempUnschedulableUntil)
	require.Equal(t, []bool{false, true}, retryProbes)
	require.Equal(t, []string{"PUT /internal/accounts/32/session", "POST /internal/accounts/32/bootstrap",
		"PUT /internal/accounts/32/session", "POST /internal/accounts/32/bootstrap"}, operations)
}

func TestPrismStatusQueryCannotEnableAccount(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		require.Equal(t, http.MethodGet, r.Method)
		_, _ = w.Write([]byte(`{"phase":"ready","ready":true,"models":["gpt-6.1-sol","gpt-6.1-sol","bad/model"],"last_heartbeat_at":1790841600}`))
	}))
	defer server.Close()
	repo := newPrismTestRepository(prismTestSource(), prismTestManaged(server.URL))
	svc := prismTestService(t, server.URL, repo, &prismTestTokens{token: prismTestJWT("latest")})
	status, err := svc.Status(context.Background(), 101)
	require.NoError(t, err)
	require.True(t, status.Ready)
	require.Equal(t, []string{"gpt-6.1-sol"}, status.Models)
	require.Equal(t, time.Unix(1790841600, 0).UTC().Format(time.RFC3339), status.LastHeartbeatAt)
	account, _ := repo.GetByID(context.Background(), 101)
	require.False(t, account.Schedulable)
	require.Equal(t, "provisioning", account.Extra["prism_phase"])
	require.EqualValues(t, 1, requests.Load())
}

func TestPrismManagementClientRejectsRedirectAndSanitizesPrivateError(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		require.Empty(t, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/?private-token=forbidden")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte(`private access_token=forbidden management-test-secret`))
	}))
	defer server.Close()
	svc := prismTestService(t, server.URL, newPrismTestRepository(), &prismTestTokens{})
	err := svc.request(context.Background(), 32, http.MethodPut, "session", map[string]any{"access_token": "private-token"}, nil)
	require.Error(t, err)
	require.EqualValues(t, 0, targetRequests.Load())
	require.NotContains(t, err.Error(), "private-token")
	require.NotContains(t, err.Error(), "management-test-secret")
	require.NotContains(t, err.Error(), "forbidden")
}

func TestPrismSessionUsesNewTokenAndRejectsInvalidSourceBeforeWorkerCalls(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		sent = append(sent, body["access_token"].(string))
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	firstToken := prismTestJWT("first")
	tokens := &prismTestTokens{token: firstToken}
	managed := prismTestManaged(server.URL)
	managed.Schedulable = true
	repo := newPrismTestRepository(prismTestSource(), managed)
	svc := prismTestService(t, server.URL, repo, tokens)
	account, _ := repo.GetByID(context.Background(), 101)
	source, _ := repo.GetByID(context.Background(), 32)
	require.NoError(t, svc.syncSession(context.Background(), account, source, false))
	require.NoError(t, svc.syncSession(context.Background(), account, source, false))
	newToken := prismTestJWT("rotated")
	tokens.mu.Lock()
	tokens.token = newToken
	tokens.mu.Unlock()
	require.NoError(t, svc.syncSession(context.Background(), account, source, false))
	require.Equal(t, []string{firstToken, newToken}, sent)
	source.Status = "inactive"
	require.NoError(t, repo.Update(context.Background(), source))
	require.Error(t, svc.EnsureSession(context.Background(), account))
	require.Len(t, sent, 2)
	account, _ = repo.GetByID(context.Background(), 101)
	require.Contains(t, account.TempUnschedulableReason, "prism:")
}

func TestPrismConcurrentSessionSyncReadsFreshTokenAfterPriorUpload(t *testing.T) {
	gate, firstUpload := make(chan struct{}), make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate) }) }
	var mu sync.Mutex
	var sent []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		sent = append(sent, body)
		first := len(sent) == 1
		mu.Unlock()
		if first {
			close(firstUpload)
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	source, managed := prismTestSource(), prismTestManaged(server.URL)
	oldToken, newToken := prismTestJWT("before-refresh"), prismTestJWT("after-refresh")
	source.Credentials["access_token"] = oldToken
	oldKey := managed.GetOpenAIProtocolAPIKey()
	newKey := strings.Repeat("new-key-", 8)
	repo := newPrismTestRepository(source, managed)
	providerEntries := make(chan int64, 2)
	tokens := prismTestTokenFunc(func(_ context.Context, account *Account) (string, error) {
		providerEntries <- account.ID
		return account.GetCredential("access_token"), nil
	})
	svc := NewPrismAccountService(repo, &prismTestAdmin{repo: repo}, tokens,
		PrismRuntimeConfig{URL: server.URL, ManagementKey: "management-test-secret"})
	t.Cleanup(svc.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	t.Cleanup(func() { unblock(); cancel(); workers.Wait() })
	workers.Add(1)
	go func() { defer workers.Done(); results <- svc.syncSession(ctx, managed, source, false) }()
	select {
	case <-firstUpload:
	case <-ctx.Done():
		t.Fatal("first session upload did not start")
	}
	require.Equal(t, int64(32), <-providerEntries)
	freshSource, err := repo.GetByID(ctx, source.ID)
	require.NoError(t, err)
	freshSource.Credentials["access_token"] = newToken
	require.NoError(t, repo.Update(ctx, freshSource))
	freshManaged, err := repo.GetByID(ctx, managed.ID)
	require.NoError(t, err)
	freshManaged.Credentials["api_key"] = newKey
	require.NoError(t, repo.Update(ctx, freshManaged))

	secondStarted := make(chan struct{})
	workers.Add(1)
	go func() {
		defer workers.Done()
		close(secondStarted)
		results <- svc.syncSession(ctx, managed, source, false)
	}()
	<-secondStarted
	select {
	case <-providerEntries:
		t.Fatal("a waiting upload read the token before acquiring the source lock")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	for range 2 {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("session uploads did not finish")
		}
	}
	require.Equal(t, int64(32), <-providerEntries, "the original OAuth account remains the only refresh owner")
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, sent, 2)
	require.Equal(t, oldToken, sent[0]["access_token"])
	require.Equal(t, newToken, sent[1]["access_token"], "an old caller snapshot cannot overwrite a refreshed token")
	require.Equal(t, oldKey, sent[0]["api_key"])
	require.Equal(t, newKey, sent[1]["api_key"], "session synchronization must use the current managed account key")
}

func TestPrismManagedConnectionCannotBeForgedOrRebound(t *testing.T) {
	managed := prismTestManaged("http://127.0.0.1:8319")
	ordinary := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	for _, input := range []*UpdateAccountInput{
		{Extra: map[string]any{"provider_preset": PrismProviderPreset}},
		{Extra: map[string]any{PrismSourceAccountKey: int64(32)}},
	} {
		require.Error(t, validatePrismAccountUpdate(ordinary, input))
	}
	for _, input := range []*UpdateAccountInput{
		{Extra: map[string]any{PrismSourceAccountKey: int64(99)}},
		{Credentials: map[string]any{"base_url": "https://attacker.example.test"}},
		{Credentials: map[string]any{"api_key": "rebound-key"}},
		{Credentials: map[string]any{"refresh_token": "copied-refresh"}},
		{Type: AccountTypeOAuth},
		{Credentials: map[string]any{"model_mapping": map[string]any{"fake-model": "fake-model"}}},
	} {
		require.Error(t, validatePrismAccountUpdate(managed, input))
	}
	require.NoError(t, validatePrismAccountUpdate(managed, &UpdateAccountInput{Name: "rename"}))
	require.Equal(t, int64(32), managed.PrismSourceAccountID())
}

func TestPrismStatusWorkerFailureCannotReportStoredReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private worker credentials", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	managed := prismTestManaged(server.URL)
	managed.Extra["prism_phase"] = "ready"
	managed.Extra["prism_models"] = []string{"gpt-6.1-sol"}
	repo := newPrismTestRepository(prismTestSource(), managed)
	svc := prismTestService(t, server.URL, repo, &prismTestTokens{token: prismTestJWT("latest")})
	status, err := svc.Status(context.Background(), managed.ID)
	require.NoError(t, err)
	require.False(t, status.Ready)
	require.NotEqual(t, "ready", status.Phase)
	require.NotContains(t, status.ErrorCode, "private")
	account, _ := repo.GetByID(context.Background(), managed.ID)
	require.False(t, account.Schedulable)
}

func TestPrismTokenProviderFailureDoesNotTransmitStoredCredentials(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	managed := prismTestManaged(server.URL)
	managed.Schedulable = true
	repo := newPrismTestRepository(prismTestSource(), managed)
	svc := prismTestService(t, server.URL, repo, &prismTestTokens{err: errors.New("private-refresh-token")})
	account, _ := repo.GetByID(context.Background(), 101)
	err := svc.EnsureSession(context.Background(), account)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private-refresh-token")
	require.EqualValues(t, 0, calls.Load())
}

func TestPrismBulkUpdateRejectsManagedConnectionChangesBeforeAnyWrite(t *testing.T) {
	proxyID, concurrency, enable := int64(1), 2, true
	for _, test := range []struct {
		name    string
		managed bool
		input   BulkUpdateAccountsInput
	}{
		{name: "forged_provider", input: BulkUpdateAccountsInput{Extra: map[string]any{"provider_preset": PrismProviderPreset}}},
		{name: "forged_source", input: BulkUpdateAccountsInput{Extra: map[string]any{PrismSourceAccountKey: int64(32)}}},
		{name: "changed_endpoint", managed: true, input: BulkUpdateAccountsInput{Credentials: map[string]any{"base_url": "https://other.example.test/v1"}}},
		{name: "changed_api_key", managed: true, input: BulkUpdateAccountsInput{Credentials: map[string]any{"api_key": "copied-secret"}}},
		{name: "copied_refresh", managed: true, input: BulkUpdateAccountsInput{Credentials: map[string]any{"refresh_token": "copied-refresh"}}},
		{name: "changed_model_catalog", managed: true, input: BulkUpdateAccountsInput{Credentials: map[string]any{"model_mapping": map[string]any{"unsupported": "unsupported"}}}},
		{name: "changed_proxy", managed: true, input: BulkUpdateAccountsInput{ProxyID: &proxyID}},
		{name: "changed_concurrency", managed: true, input: BulkUpdateAccountsInput{Concurrency: &concurrency}},
		{name: "changed_source", managed: true, input: BulkUpdateAccountsInput{Extra: map[string]any{PrismSourceAccountKey: int64(99)}}},
		{name: "forged_ready", managed: true, input: BulkUpdateAccountsInput{Extra: map[string]any{"prism_phase": "ready"}}},
		{name: "enable_before_bootstrap", managed: true, input: BulkUpdateAccountsInput{Schedulable: &enable}},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := prismTestManaged("http://127.0.0.1:8319")
			if !test.managed {
				account.Extra = nil
			}
			repo := newPrismTestRepository(account)
			svc := &adminServiceImpl{accountRepo: repo}
			input := test.input
			input.AccountIDs = []int64{account.ID}
			_, err := svc.BulkUpdateAccounts(context.Background(), &input)
			require.Error(t, err)
			require.Zero(t, repo.bulkCalls, "reject the whole request before its bulk write")
			require.Zero(t, repo.extraCalls, "reject before changing managed pending/phase flags")
			require.Zero(t, repo.schedulableCalls)
			unchanged, _ := repo.GetByID(context.Background(), account.ID)
			require.Equal(t, prismTestClone(account), unchanged)
		})
	}
}

func TestPrismEnsureSessionRejectsStaleDisabledOrDeletedSnapshot(t *testing.T) {
	for _, state := range []string{"paused", "inactive", "deleted"} {
		t.Run(state, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer server.Close()
			snapshot := prismTestManaged(server.URL)
			snapshot.Schedulable = true
			repo := newPrismTestRepository(prismTestSource(), snapshot)
			repo.mu.Lock()
			switch state {
			case "paused":
				repo.items[snapshot.ID].Schedulable = false
			case "inactive":
				repo.items[snapshot.ID].Status = "inactive"
			case "deleted":
				delete(repo.items, snapshot.ID)
			}
			repo.mu.Unlock()
			tokens := &prismTestTokens{token: prismTestJWT("latest")}
			svc := prismTestService(t, server.URL, repo, tokens)
			require.Error(t, svc.EnsureSession(context.Background(), snapshot))
			require.Zero(t, calls.Load())
			require.Empty(t, tokens.owners, "stale scheduler snapshots cannot initiate credential work")
		})
	}
}

func TestPrismAdminCreatePersistsPausedBeforeInitialSchedulerSnapshot(t *testing.T) {
	repo := newPrismTestRepository()
	svc := &adminServiceImpl{accountRepo: repo}
	paused := false
	input := &CreateAccountInput{Name: "managed Prism", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Concurrency: 1, SkipDefaultGroupBind: true, Schedulable: &paused, prismManaged: true,
		Credentials: map[string]any{"api_key": strings.Repeat("k", 64), "base_url": "http://127.0.0.1:8319/accounts/32/v1"},
		Extra:       map[string]any{"provider_preset": PrismProviderPreset, PrismSourceAccountKey: int64(32), "prism_phase": "provisioning"}}
	account, err := svc.CreateAccount(context.Background(), input)
	require.NoError(t, err)
	require.False(t, account.Schedulable)
	require.Len(t, repo.created, 1)
	require.False(t, repo.created[0].Schedulable, "the first repository write, not a later corrective update, must be paused")
	require.Zero(t, repo.schedulableCalls)
	for _, extra := range []map[string]any{
		{"provider_preset": PrismProviderPreset},
		{PrismSourceAccountKey: int64(32)},
		{"prism_phase": "ready"},
	} {
		_, err := svc.CreateAccount(context.Background(), &CreateAccountInput{Name: "forged Prism",
			Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: extra})
		require.Error(t, err)
	}
	require.Len(t, repo.created, 1, "ordinary account creation must reject managed metadata before a write")
}

func TestPrismManualPauseCancelsWorkerAutoEnable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"phase":"ready","ready":true,"models":["gpt-6.1-sol"]}`))
	}))
	defer server.Close()
	repo := newPrismTestRepository(prismTestSource(), prismTestManaged(server.URL))
	admin := &adminServiceImpl{accountRepo: repo}
	_, err := admin.SetAccountSchedulable(context.Background(), 101, false)
	require.NoError(t, err)
	svc := prismTestService(t, server.URL, repo, &prismTestTokens{token: prismTestJWT("latest")})
	svc.syncAccount(context.Background(), 101, false)
	account, err := repo.GetByID(context.Background(), 101)
	require.NoError(t, err)
	require.False(t, account.Schedulable, "a successful worker recovery cannot undo a manual pause")
	require.Equal(t, false, account.Extra["prism_auto_enable_pending"])
	require.Equal(t, "ready", account.Extra["prism_phase"])
}

func prismValidatorContext(path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	return c, w
}

func TestPrismGatewayValidatorPassesEveryWellFormedRequestUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name, body string
		chat       bool
	}{
		{name: "responses_string", body: `{"model":"gpt-6.1-sol","input":"hello","reasoning":{"effort":"low"},"store":false}`},
		{name: "responses_messages", body: `{"model":"gpt-6.1-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`},
		{name: "responses_instructions", body: `{"model":"gpt-6.1-sol","input":"hello","instructions":"be concise"}`},
		{name: "responses_history", body: `{"model":"gpt-6.1-sol","input":[{"role":"user","content":"remember this"},{"role":"assistant","content":[{"type":"output_text","text":"ok"}]},{"role":"user","content":"recall it"}]}`},
		{name: "responses_tools", body: `{"model":"gpt-6.1-sol","input":"hello","tools":[{"type":"web_search"}]}`},
		{name: "responses_unknown_fields", body: `{"model":"gpt-6.1-sol","input":"hello","temperature":0.5,"service_tier":"priority","metadata":{"a":"b"},"prompt_cache_key":"k"}`},
		{name: "responses_null_previous_response_id", body: `{"model":"gpt-6.1-sol","input":"hello","previous_response_id":null}`},
		{name: "responses_codex_style", body: `{"model":"gpt-6.1-sol","instructions":"You are Codex.","stream":true,"store":false,"parallel_tool_calls":true,
			"reasoning":{"effort":"medium","summary":"auto"},"include":["reasoning.encrypted_content"],"prompt_cache_key":"session",
			"tools":[{"type":"function","name":"shell","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"file"}}}},{"type":"web_search"}],
			"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
				{"type":"reasoning","summary":[],"encrypted_content":"abc"},
				{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
				{"type":"function_call_output","call_id":"call_1","output":"a.txt\nb.txt"},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"two files"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"mention input_image and image_url literally"}]}]}`},
		{name: "chat_text", chat: true, body: `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"low","stream":true,"stream_options":{"include_usage":true}}`},
		{name: "chat_text_parts", chat: true, body: `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`},
		{name: "chat_system", chat: true, body: `{"model":"gpt-6.1-sol","messages":[{"role":"system","content":"be concise"}]}`},
		{name: "chat_empty_messages", chat: true, body: `{"model":"gpt-6.1-sol","messages":[],"max_tokens":100}`},
		{name: "chat_tools", chat: true, body: `{"model":"gpt-6.1-sol","temperature":0.2,"tool_choice":"auto",
			"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],
			"messages":[{"role":"developer","content":"be concise"},{"role":"user","content":"find it"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"result"},{"role":"user","content":"thanks"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := "/v1/responses"
			if test.chat {
				path = "/v1/chat/completions"
			}
			c, w := prismValidatorContext(path, test.body)
			require.NoError(t, validatePrismGatewayRequest(c, []byte(test.body), test.chat))
			require.Empty(t, w.Body.String(), "validation must not write a response or rewrite input")
		})
	}
}

func TestPrismGatewayValidatorRejectsMalformedBodyWithClientError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{`{bad`, `null`, `[]`, `"text"`, ``} {
		c, w := prismValidatorContext("/v1/responses", body)
		var failoverErr *UpstreamFailoverError
		err := validatePrismGatewayRequest(c, []byte(body), false)
		require.Error(t, err)
		require.False(t, errors.As(err, &failoverErr), "a malformed body is wrong for every account")
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Contains(t, w.Body.String(), "invalid_request_error")
		require.Contains(t, w.Body.String(), "invalid_json")
		require.NotContains(t, w.Body.String(), "single user text message")
	}
}

func TestPrismGatewayValidatorFailsOverRequestsPrismCannotServe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name, body string
		chat       bool
		suffix     string
		reason     GatewayFailureReason
	}{
		{name: "chat_image", chat: true, reason: PrismNonTextInputReason, body: `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/image.png"}}]}]}`},
		{name: "chat_image_after_text", chat: true, reason: PrismNonTextInputReason, body: `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`},
		{name: "chat_input_audio", chat: true, reason: PrismNonTextInputReason, body: `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`},
		{name: "chat_file", chat: true, reason: PrismNonTextInputReason, body: `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-1"}}]}]}`},
		{name: "response_image", reason: PrismNonTextInputReason, body: `{"model":"gpt-6.1-sol","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/image.png"}]}]}`},
		{name: "response_image_in_history", reason: PrismNonTextInputReason, body: `{"model":"gpt-6.1-sol","instructions":"x","tools":[{"type":"web_search"}],"input":[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]},{"role":"assistant","content":"seen"},{"role":"user","content":"and now?"}]}`},
		{name: "response_file", reason: PrismNonTextInputReason, body: `{"model":"gpt-6.1-sol","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-1"}]}]}`},
		{name: "response_audio", reason: PrismNonTextInputReason, body: `{"model":"gpt-6.1-sol","input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`},
		{name: "response_tool_output_image", reason: PrismNonTextInputReason, body: `{"model":"gpt-6.1-sol","input":[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`},
		{name: "response_previous_response_id", reason: PrismPreviousResponseUnsupportedReason, body: `{"model":"gpt-6.1-sol","input":"hello","previous_response_id":"resp_123"}`},
		{name: "response_empty_previous_response_id", reason: PrismPreviousResponseUnsupportedReason, body: `{"model":"gpt-6.1-sol","input":"hello","previous_response_id":""}`},
		{name: "compact", reason: PrismCompactUnsupportedReason, suffix: "/compact", body: `{"model":"gpt-6.1-sol","input":"hello"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := "/v1/responses" + test.suffix
			if test.chat {
				path = "/v1/chat/completions"
			}
			c, w := prismValidatorContext(path, test.body)
			err := validatePrismGatewayRequest(c, []byte(test.body), test.chat)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, test.reason, failoverErr.Reason)
			require.True(t, failoverErr.IsPrismRequestUnsupported())
			require.Equal(t, http.StatusBadRequest, failoverErr.StatusCode)
			require.Equal(t, http.StatusBadRequest, failoverErr.ClientStatusCode)
			require.NotEmpty(t, failoverErr.ClientMessage)
			require.NotContains(t, failoverErr.ClientMessage, "single user text message")
			require.True(t, failoverErr.ShouldRetryNextAccount(), "another account in the group may serve it")
			require.False(t, failoverErr.ShouldReportAccountScheduleFailure(), "the Prism account must not be penalized")
			require.False(t, failoverErr.RetryableOnSameAccount, "same-account retries lead to a temp-unschedule penalty")
			require.False(t, failoverErr.RequestScopedTransient)
			require.False(t, failoverErr.IsCredentialFailure())
			require.Empty(t, w.Body.String(), "nothing may be written before the handler picks the next account")
			require.False(t, c.Writer.Written())
			require.False(t, IsResponseCommitted(c))
		})
	}
}

func TestPrismUnsupportedRequestDoesNotCoolDownOrPenalizeTheAccount(t *testing.T) {
	failoverErr := newPrismUnsupportedRequestError(PrismNonTextInputReason, "text only")
	repo := newPrismTestRepository(prismTestManaged("http://prism.test"))
	svc := &GatewayService{accountRepo: repo}
	svc.TempUnscheduleRetryableError(context.Background(), 101, failoverErr)
	account, err := repo.GetByID(context.Background(), 101)
	require.NoError(t, err)
	require.Nil(t, account.TempUnschedulableUntil)
	_, _, eligible := classifyOpenAIAPIKeyHealthFailure(failoverErr)
	require.False(t, eligible, "the API-key health breaker must not count it")
	require.False(t, (&UpstreamFailoverError{StatusCode: http.StatusBadRequest}).IsPrismRequestUnsupported())
	require.True(t, (&UpstreamFailoverError{StatusCode: http.StatusBadRequest}).ShouldReportAccountScheduleFailure(),
		"ordinary inference failures keep their scheduler-health behavior")
}

// Prism has no upstream WebSocket, so a WS client may only be handed a Prism
// account that is bridged to HTTP. The gate is the account's own mode: an
// account still on "off" is filtered out of ingress scheduling (the client lands
// on another account) instead of reaching the forwarder, which would close the
// socket. The gateway-wide mode router flag must not matter either way.
func TestPrismAccountWebSocketIngressSchedulingFollowsItsBridgeMode(t *testing.T) {
	newPrism := func(mode string) *Account {
		prism := prismTestManaged("http://prism.test")
		prism.Concurrency = 1
		prism.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
		return prism
	}
	regular := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": "ctx_pool"}}
	for _, routerV2 := range []bool{true, false} {
		t.Run(fmt.Sprintf("mode_router_v2=%t", routerV2), func(t *testing.T) {
			newService := func(edit func(*config.GatewayOpenAIWSConfig)) *OpenAIGatewayService {
				cfg := &config.Config{}
				ws := &cfg.Gateway.OpenAIWS
				ws.Enabled, ws.APIKeyEnabled, ws.ResponsesWebsocketsV2 = true, true, true
				ws.ModeRouterV2Enabled = routerV2
				ws.IngressModeDefault = OpenAIWSIngressModeOff
				if edit != nil {
					edit(ws)
				}
				return &OpenAIGatewayService{cfg: cfg}
			}
			svc := newService(nil)
			bridged, off := newPrism("http_bridge"), newPrism("off")
			require.True(t, svc.isOpenAIAccountTransportCompatible(bridged, OpenAIUpstreamTransportResponsesWebsocketV2Ingress))
			require.False(t, svc.isOpenAIAccountTransportCompatible(off, OpenAIUpstreamTransportResponsesWebsocketV2Ingress),
				"an account that is not bridged yet keeps the old behaviour")
			for _, account := range []*Account{bridged, off} {
				require.True(t, svc.isOpenAIAccountTransportCompatible(account, OpenAIUpstreamTransportAny))
				require.True(t, svc.isOpenAIAccountTransportCompatible(account, OpenAIUpstreamTransportHTTPSSE))
			}
			require.Equal(t, routerV2, svc.isOpenAIAccountTransportCompatible(regular, OpenAIUpstreamTransportResponsesWebsocketV2Ingress),
				"control: ordinary accounts still depend on the router flag")
			for name, edit := range map[string]func(*config.GatewayOpenAIWSConfig){
				"ws_disabled":     func(ws *config.GatewayOpenAIWSConfig) { ws.Enabled = false },
				"force_http":      func(ws *config.GatewayOpenAIWSConfig) { ws.ForceHTTP = true },
				"apikey_disabled": func(ws *config.GatewayOpenAIWSConfig) { ws.APIKeyEnabled = false },
			} {
				require.False(t, newService(edit).isOpenAIAccountTransportCompatible(bridged, OpenAIUpstreamTransportResponsesWebsocketV2Ingress),
					"the gateway's WebSocket kill switch %s also applies to the bridge", name)
			}
		})
	}
}

// Accounts created before WS clients were bridged carry "off"; the background
// sync repairs them, while the normal edit paths still cannot change the value.
func TestPrismSyncHealsLegacyWebSocketModeAndAdminsCannotChangeIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"phase":"ready","ready":true,"models":["gpt-6.1-sol"]}`))
	}))
	defer server.Close()
	managed := prismTestManaged(server.URL)
	managed.Extra["openai_apikey_responses_websockets_v2_mode"] = "off"
	repo := newPrismTestRepository(prismTestSource(), managed)
	svc := prismTestService(t, server.URL, repo, &prismTestTokens{token: prismTestJWT("latest")})

	svc.syncAccount(context.Background(), 101, false)
	healed, err := repo.GetByID(context.Background(), 101)
	require.NoError(t, err)
	require.Equal(t, "http_bridge", healed.Extra["openai_apikey_responses_websockets_v2_mode"])
	require.Equal(t, "ready", healed.Extra["prism_phase"], "the repair must not disturb the regular sync")

	repo.mu.Lock()
	before := repo.extraCalls
	repo.mu.Unlock()
	svc.syncAccount(context.Background(), 101, false)
	repo.mu.Lock()
	after := repo.extraCalls
	repo.mu.Unlock()
	require.Equal(t, before+1, after, "an account that already matches is not rewritten again (only the status update runs)")

	edited := preservePrismExtra(healed, map[string]any{"openai_apikey_responses_websockets_v2_mode": "ctx_pool", "note": "kept"})
	require.Equal(t, "http_bridge", edited["openai_apikey_responses_websockets_v2_mode"], "the single-account edit path keeps the managed value")
	require.Equal(t, "kept", edited["note"])
	require.True(t, prismConnectionExtraChanged(healed, map[string]any{"openai_apikey_responses_websockets_v2_mode": "off"}),
		"the bulk edit path rejects a change")
}

func TestPrismExplicitReconnectIsNotLostWhileBackgroundBootstrapIsRunning(t *testing.T) {
	gate, started := make(chan struct{}), make(chan struct{})
	var release, markStarted sync.Once
	unblock := func() { release.Do(func() { close(gate) }) }
	var mu sync.Mutex
	var retryProbes []bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			markStarted.Do(func() { close(started) })
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(`{}`))
			return
		}
		var body struct {
			RetryProbe bool `json:"retry_probe"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		retryProbes = append(retryProbes, body.RetryProbe)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"phase":"ready","ready":true,"models":["gpt-6.1-sol"]}`))
	}))
	t.Cleanup(server.Close)
	repo := newPrismTestRepository(prismTestSource(), prismTestManaged(server.URL))
	svc := prismTestService(t, server.URL, repo, &prismTestTokens{token: prismTestJWT("latest")})
	t.Cleanup(unblock)
	svc.schedule(101, false)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("background session upload did not start")
	}
	status, err := svc.Reconnect(context.Background(), 101)
	require.NoError(t, err)
	require.False(t, status.Ready)
	unblock()
	prismTestWaitJobs(svc)
	mu.Lock()
	require.Equal(t, []bool{false, true}, retryProbes, "an explicit readiness probe retry must survive a currently running background job")
	mu.Unlock()
}

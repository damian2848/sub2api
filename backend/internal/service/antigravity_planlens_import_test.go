package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type antImportAccountsTest struct {
	AccountRepository
	rows       []Account
	writes     int
	groupRepo  *antImportGroupsTest
	proxyRepo  *antImportProxiesTest
	duringLock func()
}
type antImportGroupsTest struct {
	GroupRepository
	rows []Group
}
type antImportProxiesTest struct {
	ProxyRepository
	rows    []Proxy
	creates int
}

func (r *antImportAccountsTest) ListAllWithFilters(context.Context, string, string, string, string, int64, string) ([]Account, error) {
	return r.rows, nil
}
func (r *antImportAccountsTest) GetByID(_ context.Context, id int64) (*Account, error) {
	for i := range r.rows {
		if r.rows[i].ID == id {
			a := r.rows[i]
			a.Credentials = shallowCopyMap(a.Credentials)
			return &a, nil
		}
	}
	return nil, errors.New("missing")
}
func (r *antImportGroupsTest) ListActiveByPlatform(context.Context, string) ([]Group, error) {
	return r.rows, nil
}
func (r *antImportProxiesTest) ListAllForFallback(context.Context) ([]Proxy, error) {
	return r.rows, nil
}
func (r *antImportProxiesTest) Create(_ context.Context, p *Proxy) error {
	r.creates++
	p.ID = 91
	r.rows = append(r.rows, *p)
	return nil
}
func (r *antImportAccountsTest) WithAntigravityImportTransaction(_ context.Context, fn func(AccountRepository, GroupRepository, ProxyRepository) (int64, error)) error {
	if r.duringLock != nil {
		r.duringLock()
	}
	_, err := fn(r, r.groupRepo, r.proxyRepo)
	return err
}
func (r *antImportAccountsTest) ApplyAntigravityImportCredentials(_ context.Context, a *Account, patch map[string]any, _ int64) error {
	r.writes++
	for i := range r.rows {
		if r.rows[i].ID == a.ID {
			for k, v := range patch {
				r.rows[i].Credentials[k] = v
			}
			if AntigravityPlanlensRecoverableOAuthError(a, time.Now()) {
				r.rows[i].Status = StatusActive
				r.rows[i].ErrorMessage = ""
			}
			return nil
		}
	}
	return errors.New("missing")
}
func (r *antImportAccountsTest) CreateWithAccountGroups(_ context.Context, a *Account, groups []AccountGroup) error {
	r.writes++
	a.ID = 88
	a.AccountGroups = groups
	a.GroupIDs = []int64{groups[0].GroupID}
	r.rows = append(r.rows, *a)
	return nil
}
func (r *antImportAccountsTest) UpdateAntigravityCredentialsIfGeneration(_ context.Context, id int64, generation string, patch map[string]any) (bool, error) {
	for i := range r.rows {
		if r.rows[i].ID == id {
			if r.rows[i].GetCredential(antigravityOAuthGeneration) != generation {
				return false, nil
			}
			r.rows[i].Credentials = patch
			return true, nil
		}
	}
	return false, nil
}
func antImportTestRequest() AntigravityPlanlensImportRequest {
	var req AntigravityPlanlensImportRequest
	_ = json.Unmarshal([]byte(`{"platform":"antigravity","email":"user@example.test","aliases":["user@example.test"],"oauthAccount":{"platform":"antigravity","type":"oauth","credentials":{"email":"user@example.test","access_token":"fresh","refresh_token":"refresh","project_id":"project","token_type":"Bearer","expires_at":"9999999999","model_mapping":{"export":"export"}}}}`), &req)
	req.DeadlineAt = time.Now().Add(time.Minute).UnixMilli()
	return req
}
func antImportTestSetup() (*adminServiceImpl, *antImportAccountsTest) {
	multiplier := 1.0
	r := &antImportAccountsTest{groupRepo: &antImportGroupsTest{rows: []Group{{ID: 52, Name: "Antigravity", Status: StatusActive}, {ID: 53, Name: "Antigravity (personal@example.test)", Status: StatusActive}}}, proxyRepo: &antImportProxiesTest{}}
	r.rows = []Account{{ID: 12, Name: "user@example.test", Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{"email": "user@example.test", "access_token": "old", "model_mapping": map[string]any{"manual": "custom"}}, Extra: map[string]any{"manual": "keep"}, Concurrency: 4, Priority: 1, RateMultiplier: &multiplier, GroupRateMultiplier: &multiplier, AutoPauseOnExpired: true, GroupIDs: []int64{52}, AccountGroups: []AccountGroup{{GroupID: 52, Priority: 1}}, Status: "disabled", Schedulable: false, ErrorMessage: "manual disabled", TempUnschedulableReason: "rate limited"}}
	return &adminServiceImpl{accountRepo: r, groupRepo: r.groupRepo, proxyRepo: r.proxyRepo}, r
}

func TestAntigravityPlanlensUpsertPreservesManualSettings(t *testing.T) {
	svc, repo := antImportTestSetup()
	req := antImportTestRequest()
	before := repo.rows[0]
	before.Credentials = shallowCopyMap(before.Credentials)
	ready, err := svc.AntigravityPlanlensImport(context.Background(), req, false)
	if err != nil || ready.Status != "ready" || ready.RemoteID != "12" || repo.writes != 0 {
		t.Fatalf("prepare: %#v %v", ready, err)
	}
	req.Revision = ready.Revision
	req.RemoteID = ready.RemoteID
	req.ProxyURL = "socks5://other.example:1080"
	result, err := svc.AntigravityPlanlensImport(context.Background(), req, true)
	if err != nil || result.Status != "updated" || repo.writes != 1 {
		t.Fatalf("commit: %#v %v", result, err)
	}
	after := repo.rows[0]
	if after.GetCredential("access_token") != "fresh" || after.GetCredential(antigravityOAuthGeneration) == "" || !reflect.DeepEqual(after.Credentials["model_mapping"], before.Credentials["model_mapping"]) {
		t.Fatal("credentials merge incorrect")
	}
	after.Credentials = before.Credentials
	if !reflect.DeepEqual(after, before) {
		t.Fatal("manual settings changed")
	}
}
func TestAntigravityPlanlensRejectsConcurrentChangesAndAmbiguity(t *testing.T) {
	for _, mode := range []string{"credentials", "proxy", "disabled", "duplicate", "target", "expired"} {
		t.Run(mode, func(t *testing.T) {
			svc, r := antImportTestSetup()
			req := antImportTestRequest()
			ready, _ := svc.AntigravityPlanlensImport(context.Background(), req, false)
			req.Revision = ready.Revision
			expected := "stale_revision"
			switch mode {
			case "credentials":
				r.duringLock = func() { r.rows[0].Credentials["refresh_token"] = "rotated" }
			case "proxy":
				r.duringLock = func() { id := int64(10); r.rows[0].ProxyID = &id }
			case "disabled":
				r.duringLock = func() { r.rows[0].Status = "active" }
			case "duplicate":
				r.duringLock = func() { r.rows = append(r.rows, r.rows[0]) }
				expected = "ambiguous_identity"
			case "target":
				req.RemoteID = "99"
				expected = "identity_mismatch"
			case "expired":
				req.DeadlineAt = 1
				expected = "expired"
			}
			got, _ := svc.AntigravityPlanlensImport(context.Background(), req, true)
			if got.Status != expected || r.writes != 0 {
				t.Fatalf("got %#v, writes %d", got, r.writes)
			}
		})
	}
}
func TestAntigravityPlanlensNewUsesUniqueMainGroup(t *testing.T) {
	for _, mode := range []string{"success", "ambiguous_group", "template_disagrees", "no_template", "inactive_proxy"} {
		t.Run(mode, func(t *testing.T) {
			svc, r := antImportTestSetup()
			r.rows[0].Name = "template@example.test"
			r.rows[0].Credentials["email"] = "template@example.test"
			req := antImportTestRequest()
			req.ProxyURL = "socks5://proxy.example:1080"
			switch mode {
			case "ambiguous_group":
				r.groupRepo.rows = append(r.groupRepo.rows, Group{ID: 54, Name: "antigravity"})
			case "template_disagrees":
				copy := r.rows[0]
				copy.ID = 13
				copy.Concurrency = 10
				r.rows = append(r.rows, copy)
			case "no_template":
				r.rows[0].GroupIDs = []int64{53}
			case "inactive_proxy":
				r.proxyRepo.rows = []Proxy{{ID: 9, Protocol: "socks5", Host: "proxy.example", Port: 1080, Status: "disabled"}}
			}
			ready, _ := svc.AntigravityPlanlensImport(context.Background(), req, false)
			if mode != "success" {
				if ready.Status != "import_configuration_missing" || r.writes != 0 {
					t.Fatalf("got %#v", ready)
				}
				return
			}
			if ready.Status != "ready" || ready.RemoteID != "" {
				t.Fatalf("got %#v", ready)
			}
			req.Revision = ready.Revision
			got, _ := svc.AntigravityPlanlensImport(context.Background(), req, true)
			if got.Status != "imported" || r.writes != 1 || r.proxyRepo.creates != 1 {
				t.Fatalf("got %#v", got)
			}
			newAccount := r.rows[1]
			if newAccount.GroupIDs[0] != 52 || newAccount.Concurrency != 4 || newAccount.Status != StatusActive || !newAccount.Schedulable || newAccount.ProxyID == nil || newAccount.Credentials["model_mapping"] == nil {
				t.Fatal("new account configuration incorrect")
			}
		})
	}
}
func TestAntigravityPlanlensLateRefreshCannotReplaceNewAuthorization(t *testing.T) {
	_, repo := antImportTestSetup()
	repo.rows[0].Credentials[antigravityOAuthGeneration] = "new"
	repo.rows[0].Credentials["access_token"] = "new"
	old := &Account{ID: 12, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "old"}}
	if err := persistAccountCredentials(context.Background(), repo, old, map[string]any{"access_token": "late"}); err != nil {
		t.Fatal(err)
	}
	if repo.rows[0].GetCredential("access_token") != "new" || old.GetCredential("access_token") != "new" {
		t.Fatal("old refresh replaced new authorization")
	}
	if AntigravityTokenCacheKey(old) == AntigravityTokenCacheKey(&Account{ID: 12}) {
		t.Fatal("generation not isolated")
	}
}

type antImportBlockingRefresh struct {
	AntigravityTokenRefresher
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (r *antImportBlockingRefresh) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	r.calls.Add(1)
	r.entered <- struct{}{}
	select {
	case <-r.release:
		return MergeCredentials(account.Credentials, map[string]any{"access_token": "rotated"}), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestAntigravityPlanlensRefreshLockSurvivesAuthorizationReread(t *testing.T) {
	_, repo := antImportTestSetup()
	repo.rows[0].Status = StatusActive
	repo.rows[0].Credentials[antigravityOAuthGeneration] = "new"
	repo.rows[0].Credentials["expires_at"] = strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
	current, err := repo.GetByID(context.Background(), 12)
	if err != nil {
		t.Fatal(err)
	}
	stale := *current
	stale.Credentials = shallowCopyMap(current.Credentials)
	stale.Credentials[antigravityOAuthGeneration] = "old"
	if AntigravityTokenCacheKey(&stale) == AntigravityTokenCacheKey(current) {
		t.Fatal("access-token cache must remain isolated by authorization")
	}
	executor := &antImportBlockingRefresh{entered: make(chan struct{}, 2), release: make(chan struct{})}
	api := NewOAuthRefreshAPI(repo, nil)
	firstDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		_, err := api.RefreshIfNeeded(ctx, &stale, executor, 15*time.Minute)
		firstDone <- err
	}()
	select {
	case <-executor.entered:
	case <-ctx.Done():
		t.Fatal("initial refresh did not reach the provider")
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, secondErr := api.RefreshIfNeeded(secondCtx, current, executor, 15*time.Minute)
	secondCancel()
	close(executor.release)
	if firstErr := <-firstDone; firstErr != nil {
		t.Fatal(firstErr)
	}
	if !errors.Is(secondErr, context.DeadlineExceeded) || executor.calls.Load() != 1 {
		t.Fatalf("new authorization refreshed outside the existing account lock: calls=%d, error=%v", executor.calls.Load(), secondErr)
	}
}
func TestAntigravityPlanlensOnlyClearsNativeUnblockedAuthErrors(t *testing.T) {
	for _, mode := range []string{"invalid_grant", "missing_project", "manual_false", "manual_disabled", "policy", "invalid_client", "unknown", "cooldown", "rate_limit", "overload", "expired_account"} {
		t.Run(mode, func(t *testing.T) {
			svc, r := antImportTestSetup()
			r.rows[0].Status = StatusError
			r.rows[0].Schedulable = true
			r.rows[0].ErrorMessage = "Token refresh failed (non-retryable): invalid_grant: token expired"
			future := time.Now().Add(time.Hour)
			past := time.Now().Add(-time.Hour)
			switch mode {
			case "missing_project":
				r.rows[0].ErrorMessage = "Token refresh failed (non-retryable): missing_project_id: unavailable"
			case "manual_false":
				r.rows[0].Schedulable = false
			case "manual_disabled":
				r.rows[0].Status = "disabled"
			case "policy":
				r.rows[0].ErrorMessage += " account disabled due to policy"
			case "invalid_client":
				r.rows[0].ErrorMessage = "Token refresh failed (non-retryable): invalid_client"
			case "unknown":
				r.rows[0].ErrorMessage = "manual invalid_grant review"
			case "cooldown":
				r.rows[0].TempUnschedulableUntil = &future
			case "rate_limit":
				r.rows[0].RateLimitResetAt = &future
			case "overload":
				r.rows[0].OverloadUntil = &future
			case "expired_account":
				r.rows[0].ExpiresAt = &past
			}
			beforeStatus, beforeMessage := r.rows[0].Status, r.rows[0].ErrorMessage
			req := antImportTestRequest()
			ready, _ := svc.AntigravityPlanlensImport(context.Background(), req, false)
			req.Revision = ready.Revision
			result, _ := svc.AntigravityPlanlensImport(context.Background(), req, true)
			if result.Status != "updated" {
				t.Fatalf("got %#v", result)
			}
			if mode == "invalid_grant" || mode == "missing_project" {
				if r.rows[0].Status != StatusActive || r.rows[0].ErrorMessage != "" {
					t.Fatal("native auth error not cleared")
				}
			} else if r.rows[0].Status != beforeStatus || r.rows[0].ErrorMessage != beforeMessage {
				t.Fatal("unrelated block was cleared")
			}
		})
	}
}

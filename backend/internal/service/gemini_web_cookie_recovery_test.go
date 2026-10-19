package service

import (
	"context"
	"testing"
	"time"
)

type recoveryRepoTest struct {
	AccountRepository
	account Account
	count   int
	applied bool
	writes  int
}

func (r *recoveryRepoTest) GetByID(context.Context, int64) (*Account, error) {
	a := r.account
	a.Credentials = shallowCopyMap(a.Credentials)
	return &a, nil
}
func (r *recoveryRepoTest) ListAllWithFilters(context.Context, string, string, string, string, int64, string) ([]Account, error) {
	out := []Account{}
	for i := 0; i < r.count; i++ {
		out = append(out, r.account)
	}
	return out, nil
}
func (r *recoveryRepoTest) ApplyGeminiWebCookieRecovery(_ context.Context, _ *Account, cookie, generation string, _ int64) (bool, error) {
	r.writes++
	if !r.applied {
		return false, nil
	}
	r.account.Credentials["gemini_web_cookie"] = cookie
	r.account.Credentials[geminiWebCookieGeneration] = generation
	r.account.TempUnschedulableReason = ""
	return true, nil
}
func (r *recoveryRepoTest) UpdateGeminiWebCredentials(_ context.Context, _ int64, generation string, credentials map[string]any) (bool, error) {
	if generation != r.account.GetCredential(geminiWebCookieGeneration) {
		return false, nil
	}
	r.writes++
	r.account.Credentials = shallowCopyMap(credentials)
	return true, nil
}

func TestRecoveryGenerationServiceGuardsAndSuccess(t *testing.T) {
	cookie := `[{"name":"__Secure-1PSID","value":"samepsid1234567890","domain":".google.com","path":"/","secure":true,"session":true},{"name":"__Secure-1PSIDTS","value":"new","domain":".google.com","path":"/","secure":true,"session":true}]`
	for _, mode := range []string{"success", "expired_cookie", "disabled", "identity", "duplicate", "other_error", "stale", "expired", "cas_conflict"} {
		t.Run(mode, func(t *testing.T) {
			repo := &recoveryRepoTest{count: 1, applied: true, account: Account{ID: 12, Name: "gemini-web-user@example.test", Platform: PlatformGemini, Type: AccountTypeGeminiWeb, Status: StatusActive, Schedulable: true, TempUnschedulableReason: GeminiWebCookieRotationRejected, Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=samepsid1234567890; __Secure-1PSIDTS=old", "model_mapping": "preserved"}}}
			if mode == "expired_cookie" {
				repo.account.TempUnschedulableReason = GeminiWebCookieExpiredOrRejected
			}
			svc := &adminServiceImpl{accountRepo: repo}
			ready, err := svc.GeminiWebCookieRecovery(context.Background(), 12, "user@example.test", "", "", 0)
			if err != nil || ready.Status != "ready" {
				t.Fatalf("prepare: %#v %v", ready, err)
			}
			rev := ready.Revision
			email := "user@example.test"
			deadline := time.Now().Add(time.Minute).UnixMilli()
			expected := "recovered"
			switch mode {
			case "disabled":
				repo.account.Status = "inactive"
				expected = "disabled"
			case "identity":
				email = "other@example.test"
				expected = "identity_mismatch"
			case "duplicate":
				repo.count = 2
				expected = "ambiguous_identity"
			case "other_error":
				repo.account.TempUnschedulableReason = "rate limited"
				expected = "not_rejected"
			case "stale":
				repo.account.Credentials["model_mapping"] = "edited"
				expected = "stale_revision"
			case "expired":
				deadline = 1
				expected = "expired"
			case "cas_conflict":
				repo.applied = false
				expected = "stale_revision"
			}
			result, err := svc.GeminiWebCookieRecovery(context.Background(), 12, email, rev, cookie, deadline)
			if err != nil || result.Status != expected {
				t.Fatalf("got %#v %v, want %s", result, err, expected)
			}
			if expected != "recovered" && mode != "cas_conflict" && repo.writes != 0 {
				t.Fatal("unsafe write")
			}
			if expected == "recovered" && (repo.account.GetCredential(geminiWebCookieGeneration) == "" || repo.account.GetCredential("model_mapping") != "preserved") {
				t.Fatal("generation/configuration lost")
			}
		})
	}
}

func TestRecoveryGenerationRejectsLateCredentialPersistence(t *testing.T) {
	repo := &recoveryRepoTest{account: Account{ID: 12, Platform: PlatformGemini, Type: AccountTypeGeminiWeb, Credentials: map[string]any{"gemini_web_cookie": "fresh", geminiWebCookieGeneration: "new"}}}
	old := &Account{ID: 12, Platform: PlatformGemini, Type: AccountTypeGeminiWeb, Credentials: map[string]any{"gemini_web_cookie": "old"}}
	if err := persistAccountCredentials(context.Background(), repo, old, map[string]any{"gemini_web_cookie": "late"}); err != nil {
		t.Fatal(err)
	}
	if repo.writes != 0 || repo.account.GetCredential("gemini_web_cookie") != "fresh" {
		t.Fatal("old response overwrote recovery")
	}
}

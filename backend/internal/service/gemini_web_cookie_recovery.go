package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

const GeminiWebCookieRecoveryCapability = "gemini-web-cookie-recovery-v1"
const GeminiWebCookieRotationRejected = "Gemini Web cookie rotation rejected"
const GeminiWebCookieExpiredOrRejected = "Gemini Web cookie expired or rejected"

func IsGeminiWebCookieRejection(reason string) bool {
	return reason == GeminiWebCookieRotationRejected || reason == GeminiWebCookieExpiredOrRejected
}

const geminiWebCookieGeneration = "gemini_web_cookie_generation"

type GeminiWebCookieRecoveryResult struct {
	Capability string `json:"capability"`
	OK         bool   `json:"ok"`
	Status     string `json:"status"`
	Revision   string `json:"revision,omitempty"`
	Applied    bool   `json:"-"`
}

func cookieRecoveryResult(status string) *GeminiWebCookieRecoveryResult {
	return &GeminiWebCookieRecoveryResult{Capability: GeminiWebCookieRecoveryCapability, OK: status == "ready" || status == "recovered", Status: status}
}

func geminiWebRecoveryEmail(a *Account) string {
	if a == nil {
		return ""
	}
	email := strings.TrimSpace(a.GetCredential("email"))
	if email == "" {
		email, _ = a.Extra["email"].(string)
		email = strings.TrimSpace(email)
	}
	if email == "" {
		email = strings.TrimSpace(a.Name)
		if strings.HasPrefix(strings.ToLower(email), "gemini-web-") {
			email = email[len("gemini-web-"):]
		}
		email = strings.TrimSuffix(email, ".json")
	}
	return strings.ToLower(email)
}

// Exclude activity timestamps: quota polling must not invalidate an export lease.
// Include all credentials and the exact identity/rejection/configuration guards.
func geminiWebRecoveryRevision(a *Account) string {
	payload, _ := json.Marshal([]any{a.ID, a.Name, a.Platform, a.Type, a.Credentials, geminiWebRecoveryEmail(a), a.Status, a.Schedulable, a.ErrorMessage, a.TempUnschedulableReason})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

type geminiWebCookieRecoveryRepository interface {
	ApplyGeminiWebCookieRecovery(context.Context, *Account, string, string, int64) (bool, error)
}

// GeminiWebCookieRecovery performs a read-only prepare when cookieJSON is empty.
// Recovery never enables an administratively disabled/unschedulable account.
func (s *adminServiceImpl) GeminiWebCookieRecovery(ctx context.Context, id int64, email, revision, cookieJSON string, deadlineAt int64) (*GeminiWebCookieRecoveryResult, error) {
	repo, capable := s.accountRepo.(geminiWebCookieRecoveryRepository)
	if !capable {
		return cookieRecoveryResult("capability_missing"), nil
	}
	if id <= 0 || !strings.Contains(email, "@") || len(email) > 254 {
		return cookieRecoveryResult("invalid_request"), nil
	}
	email = strings.ToLower(strings.TrimSpace(email))
	lock := geminiWebCredentialPersistLock(id)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return cookieRecoveryResult("expired"), nil
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if account.Platform != PlatformGemini || account.Type != AccountTypeGeminiWeb || geminiWebRecoveryEmail(account) != email {
		return cookieRecoveryResult("identity_mismatch"), nil
	}
	if account.Status != StatusActive || !account.Schedulable {
		return cookieRecoveryResult("disabled"), nil
	}
	if account.ErrorMessage != "" || !IsGeminiWebCookieRejection(account.TempUnschedulableReason) {
		return cookieRecoveryResult("not_rejected"), nil
	}
	// Email aliases or duplicate imports must not let one profile replace two rows.
	accounts, err := s.accountRepo.ListAllWithFilters(ctx, PlatformGemini, AccountTypeGeminiWeb, "", "", 0, "")
	if err != nil {
		return nil, err
	}
	matches := 0
	for i := range accounts {
		if geminiWebRecoveryEmail(&accounts[i]) == email {
			matches++
		}
	}
	if matches != 1 {
		return cookieRecoveryResult("ambiguous_identity"), nil
	}
	currentRevision := geminiWebRecoveryRevision(account)
	if cookieJSON == "" {
		result := cookieRecoveryResult("ready")
		result.Revision = currentRevision
		return result, nil
	}
	if deadlineAt <= time.Now().UnixMilli() || deadlineAt > time.Now().Add(5*time.Minute).UnixMilli() {
		return cookieRecoveryResult("expired"), nil
	}
	if currentRevision != revision {
		return cookieRecoveryResult("stale_revision"), nil
	}
	var jar []map[string]any
	if len(cookieJSON) > 512*1024 || json.Unmarshal([]byte(cookieJSON), &jar) != nil || len(jar) == 0 {
		return cookieRecoveryResult("invalid_request"), nil
	}
	credentials := map[string]any{"gemini_web_cookie": cookieJSON}
	if ValidateGeminiWebCredentials(PlatformGemini, AccountTypeGeminiWeb, credentials) != nil {
		return cookieRecoveryResult("invalid_request"), nil
	}
	if geminiweb.NormalizeCookieHeader(cookieJSON) == geminiweb.NormalizeCookieHeader(account.GetCredential("gemini_web_cookie")) {
		return cookieRecoveryResult("unchanged_cookie"), nil
	}
	generationBytes := make([]byte, 16)
	if _, err = rand.Read(generationBytes); err != nil {
		return nil, err
	}
	generation := hex.EncodeToString(generationBytes)
	applied, err := repo.ApplyGeminiWebCookieRecovery(ctx, account, cookieJSON, generation, deadlineAt)
	if err != nil {
		result := cookieRecoveryResult("verification_failed")
		result.Applied = applied
		return result, nil
	}
	if !applied {
		return cookieRecoveryResult("stale_revision"), nil
	}
	if s.runtimeBlocker != nil {
		s.runtimeBlocker.ClearAccountSchedulingBlock(id)
	}
	latest, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		result := cookieRecoveryResult("verification_failed")
		result.Applied = true
		return result, nil
	}
	if latest.GetCredential(geminiWebCookieGeneration) != generation || latest.GetCredential("gemini_web_cookie") != cookieJSON || latest.TempUnschedulableReason != "" || latest.Status != StatusActive || !latest.Schedulable {
		result := cookieRecoveryResult("verification_failed")
		result.Applied = true
		return result, nil
	}
	result := cookieRecoveryResult("recovered")
	result.Applied = true
	return result, nil
}

// A delayed rejection from a request started before recovery is not evidence
// about the replacement session and must not block that replacement account.
func geminiWebCredentialIsCurrent(ctx context.Context, repo AccountRepository, account *Account) bool {
	if repo == nil || account == nil {
		return false
	}
	latest, err := repo.GetByID(ctx, account.ID)
	return err == nil && latest != nil && latest.GetCredential(geminiWebCookieGeneration) == account.GetCredential(geminiWebCookieGeneration)
}

func markGeminiWebCookieRejected(ctx context.Context, repo AccountRepository, account *Account, reason string) {
	if account == nil || repo == nil {
		return
	}
	lock := geminiWebCredentialPersistLock(account.ID)
	lock.Lock()
	defer lock.Unlock()
	until := time.Now().Add(10 * time.Minute)
	if setter, ok := repo.(interface {
		SetGeminiWebTempUnschedulable(context.Context, int64, string, time.Time, string) (bool, error)
	}); ok {
		applied, err := setter.SetGeminiWebTempUnschedulable(ctx, account.ID, account.GetCredential(geminiWebCookieGeneration), until, reason)
		if err == nil && applied {
			account.TempUnschedulableUntil = &until
		}
		return
	}
	if !geminiWebCredentialIsCurrent(ctx, repo, account) {
		return
	}
	if repo.SetTempUnschedulable(ctx, account.ID, until, reason) == nil {
		account.TempUnschedulableUntil = &until
	}
}

// Only the captured cookie-rejection cache state may be removed. A new error
// posted concurrently has a different value and survives the Redis CAS.
type geminiCookieRecoveryCache interface {
	DeleteTempUnschedIfMatches(context.Context, int64, *TempUnschedState) error
}

func (s *RateLimitService) PrepareGeminiWebCookieRecoveryCache(ctx context.Context, id int64) (*TempUnschedState, error) {
	if s.tempUnschedCache == nil {
		return nil, nil
	}
	if _, ok := s.tempUnschedCache.(geminiCookieRecoveryCache); !ok {
		return nil, errors.New("cookie recovery cache unsupported")
	}
	return s.tempUnschedCache.GetTempUnsched(ctx, id)
}

func (s *RateLimitService) FinalizeGeminiWebCookieRecovery(ctx context.Context, id int64, expected *TempUnschedState) error {
	if s.tempUnschedCache != nil {
		cache, ok := s.tempUnschedCache.(geminiCookieRecoveryCache)
		if !ok {
			return errors.New("cookie recovery cache unsupported")
		}
		if err := cache.DeleteTempUnschedIfMatches(ctx, id, expected); err != nil {
			return err
		}
	}
	s.notifyAccountSchedulingBlockCleared(id)
	return nil
}

func geminiWebCacheCredential(account *Account, cookie string) geminiweb.Credential {
	authUser, authUserSet := geminiWebAuthUser(account)
	return geminiweb.Credential{Cookie: cookie, AuthUser: authUser, AuthUserSet: authUserSet, CacheGeneration: account.GetCredential(geminiWebCookieGeneration)}
}

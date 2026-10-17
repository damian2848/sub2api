package service

import (
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

// SanitizeStoredCredentials strips secrets that must never be persisted on the
// account credentials map after conversion to OAuth tokens (Grok Web SSO / password).
// Call from admin create/update/import/apply-oauth paths.
//
// Browser cookies are retained only by SanitizeStoredCredentialsForAccount;
// this legacy helper intentionally strips them from generic conversion paths.
func SanitizeStoredCredentials(platform string, creds map[string]any) map[string]any {
	if creds == nil {
		return nil
	}
	keys := []string{
		"password", "sso_token", "sso", "sso-rw", "clearTextPassword",
	}
	for _, key := range keys {
		delete(creds, key)
	}
	delete(creds, "cookie")
	delete(creds, "gemini_web_cookie")
	delete(creds, "gemini_web_cookies")
	return creds
}

// SanitizeStoredCredentialsForAccount preserves the one browser-cookie field
// used by the explicit Gemini Web provider account while retaining the old
// secret-scrubbing behavior for every other account type.
func SanitizeStoredCredentialsForAccount(platform, accountType string, creds map[string]any) map[string]any {
	if creds == nil {
		return nil
	}
	webCookie := creds["gemini_web_cookie"]
	SanitizeStoredCredentials(platform, creds)
	if platform == PlatformGemini && accountType == AccountTypeGeminiWeb && webCookie != nil {
		creds["gemini_web_cookie"] = webCookie
	}
	return creds
}

func ValidateGeminiWebCredentials(platform, accountType string, creds map[string]any) error {
	if accountType == AccountTypeGeminiWeb && platform != PlatformGemini {
		return errors.New("gemini_web accounts must use the gemini platform")
	}
	if platform != PlatformGemini || accountType != AccountTypeGeminiWeb {
		return nil
	}
	value, _ := creds["gemini_web_cookie"].(string)
	normalized := geminiweb.NormalizeCookieHeader(value)
	if len(normalized) < 20 || !strings.Contains(normalized, "__Secure-1PSID=") {
		return errors.New("gemini_web accounts require a browser Cookie header containing __Secure-1PSID")
	}
	return nil
}

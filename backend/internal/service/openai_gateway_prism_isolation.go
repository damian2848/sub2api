package service

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// The sidecar bearer identifies a source account, not the downstream user. Pass
// an authenticated tenant namespace separately; never accept caller-controlled
// X-Prism-* headers or reuse a project based on shared/derived prompt content.
func setPrismIsolationHeaders(c *gin.Context, account *Account, headers http.Header, bodies ...[]byte) {
	for key := range headers {
		if strings.EqualFold(key, "X-Prism-Key-Scope") || strings.EqualFold(key, "X-Prism-Session-Scope") {
			delete(headers, key)
		}
	}
	if account == nil || !account.IsManagedPrismAccount() {
		return
	}
	id := getAPIKeyIDFromContext(c)
	if id <= 0 {
		return // probes and missing auth contexts get a fresh project per request
	}
	key := strconv.FormatInt(id, 10)
	headers.Set("X-Prism-Key-Scope", prismScopeDigest("prism-key:v1:"+key))
	var session string
	if c != nil && c.Request != nil {
		for _, name := range []string{"X-Claude-Code-Session-Id", "X-OpenCode-Session", "X-Codex-Session-Id", "session_id", "conversation_id"} {
			if session = sanitizeSessionID(c.GetHeader(name)); session != "" {
				break
			}
		}
	}
	if session == "" {
		payloads := openCodeInboundBodies(c)
		if len(payloads) == 0 {
			payloads = bodies
		}
		// A converted body may contain a gateway-derived prefix cache key. If
		// original input is available, missing session there MUST remain missing.
		for _, body := range payloads {
			// client_metadata is retained by the WebSocket-to-HTTP bridge. Plain
			// metadata.user_id is NOT a reliable conversation identifier.
			for _, path := range []string{"prompt_cache_key", "client_metadata.session_id", "client_metadata.thread_id"} {
				value := gjson.GetBytes(body, path)
				if value.Type == gjson.String {
					session = sanitizeSessionID(value.String())
				}
				if session != "" {
					break
				}
			}
			if session == "" {
				session = prismClaudeSessionID(body)
			}
			if session != "" {
				break
			}
		}
	}
	if session != "" {
		headers.Set("X-Prism-Session-Scope", prismScopeDigest("prism-session:v1:"+key+":"+session))
	}
}

func prismScopeDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// Wrongly typed/default fields (false, [], {}) are not conversation identities.
func prismClaudeSessionID(body []byte) string {
	value := gjson.GetBytes(body, "metadata.user_id")
	if value.Type != gjson.String {
		return ""
	}
	userID := strings.TrimSpace(value.String())
	if strings.HasPrefix(userID, "{") {
		embedded := gjson.Get(userID, "session_id")
		if embedded.Type != gjson.String {
			return ""
		}
		return sanitizeSessionID(embedded.String())
	}
	if matches := claudeCodeSessionSuffixPattern.FindStringSubmatch(userID); len(matches) >= 2 {
		return sanitizeSessionID(matches[1])
	}
	return ""
}

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
)

// SettingKeyGeminiWebCannedReplies stores the operator's extra canned failure
// phrases as a JSON array of strings. The built-in phrases are not stored.
const SettingKeyGeminiWebCannedReplies = "gemini_web_canned_replies"

const (
	geminiWebCannedReplyMaxPhrases = 50
	geminiWebCannedReplyMaxChars   = 300
	geminiWebCannedReplyCacheTTL   = 60 * time.Second
)

// GeminiWebCannedReplySettings lists the canned failure messages that fail a
// Gemini Web reply. BuiltIn is read-only; Custom is what the operator edits.
type GeminiWebCannedReplySettings struct {
	BuiltIn []string `json:"built_in"`
	Custom  []string `json:"custom"`
}

type cachedGeminiWebCannedReplies struct {
	matcher   geminiweb.CannedReplies
	expiresAt time.Time
}

// GetGeminiWebCannedReplySettings returns the built-in and custom phrases.
func (s *SettingService) GetGeminiWebCannedReplySettings(ctx context.Context) (*GeminiWebCannedReplySettings, error) {
	custom, err := s.loadGeminiWebCannedReplies(ctx)
	if err != nil {
		return nil, err
	}
	return &GeminiWebCannedReplySettings{BuiltIn: geminiweb.BuiltInCannedReplies(), Custom: custom}, nil
}

// SetGeminiWebCannedReplySettings validates and stores the custom phrases.
// Phrases are trimmed; blank ones and duplicates (including copies of a
// built-in phrase) are dropped. The new list applies to the next request.
func (s *SettingService) SetGeminiWebCannedReplySettings(ctx context.Context, custom []string) ([]string, error) {
	if s == nil || s.settingRepo == nil {
		return nil, errors.New("setting service is not configured")
	}
	cleaned, err := normalizeGeminiWebCannedReplyPhrases(custom)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(cleaned)
	if err := s.settingRepo.Set(ctx, SettingKeyGeminiWebCannedReplies, string(encoded)); err != nil {
		return nil, fmt.Errorf("save Gemini Web canned replies: %w", err)
	}
	s.geminiWebCannedRepliesCache.Store(&cachedGeminiWebCannedReplies{
		matcher: geminiweb.NewCannedReplies(cleaned), expiresAt: time.Now().Add(geminiWebCannedReplyCacheTTL),
	})
	return cleaned, nil
}

// GeminiWebCannedReplies returns the matcher for the built-in and custom
// phrases. It is cached for a minute; when the setting cannot be read it falls
// back to the built-in phrases so requests are never blocked on it.
func (s *SettingService) GeminiWebCannedReplies(ctx context.Context) geminiweb.CannedReplies {
	if s == nil {
		return geminiweb.CannedReplies{}
	}
	if cached, ok := s.geminiWebCannedRepliesCache.Load().(*cachedGeminiWebCannedReplies); ok && cached != nil && time.Now().Before(cached.expiresAt) {
		return cached.matcher
	}
	custom, err := s.loadGeminiWebCannedReplies(ctx)
	ttl := geminiWebCannedReplyCacheTTL
	if err != nil {
		custom, ttl = nil, 5*time.Second
	}
	matcher := geminiweb.NewCannedReplies(custom)
	s.geminiWebCannedRepliesCache.Store(&cachedGeminiWebCannedReplies{matcher: matcher, expiresAt: time.Now().Add(ttl)})
	return matcher
}

func (s *SettingService) loadGeminiWebCannedReplies(ctx context.Context) ([]string, error) {
	if s == nil || s.settingRepo == nil {
		return []string{}, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyGeminiWebCannedReplies)
	if errors.Is(err, ErrSettingNotFound) || (err == nil && strings.TrimSpace(raw) == "") {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get Gemini Web canned replies: %w", err)
	}
	var custom []string
	if err := json.Unmarshal([]byte(raw), &custom); err != nil {
		return nil, fmt.Errorf("parse Gemini Web canned replies: %w", err)
	}
	if custom == nil {
		custom = []string{}
	}
	return custom, nil
}

func normalizeGeminiWebCannedReplyPhrases(phrases []string) ([]string, error) {
	seen := make(map[string]struct{}, len(phrases))
	for _, builtIn := range geminiweb.BuiltInCannedReplies() {
		seen[geminiweb.NormalizeCannedReply(builtIn)] = struct{}{}
	}
	cleaned := make([]string, 0, len(phrases))
	for _, phrase := range phrases {
		phrase = strings.TrimSpace(phrase)
		if phrase == "" {
			continue
		}
		if utf8.RuneCountInString(phrase) > geminiWebCannedReplyMaxChars {
			return nil, infraerrors.BadRequest("INVALID_GEMINI_WEB_CANNED_REPLY",
				fmt.Sprintf("each phrase must be at most %d characters", geminiWebCannedReplyMaxChars))
		}
		key := geminiweb.NormalizeCannedReply(phrase)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		cleaned = append(cleaned, phrase)
	}
	if len(cleaned) > geminiWebCannedReplyMaxPhrases {
		return nil, infraerrors.BadRequest("INVALID_GEMINI_WEB_CANNED_REPLY",
			fmt.Sprintf("at most %d custom phrases are allowed", geminiWebCannedReplyMaxPhrases))
	}
	return cleaned, nil
}

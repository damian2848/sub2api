package geminiweb

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrCannedReply means Gemini Web replaced its answer with one of the fixed
// messages the web app shows when generation fails. The HTTP response is a
// normal 200 with the message as answer text, so callers must check for it.
var ErrCannedReply = errors.New("gemini web replied with a canned error message")

// builtInCannedReplies are the whole-answer fallbacks seen from Gemini Web in
// place of a failed generation. Operators can add more (see CannedReplies).
var builtInCannedReplies = []string{
	"I seem to be encountering an error. Can I try something else for you?",
	"I'm having a hard time fulfilling your request. Can I help you with something else instead?",
	"I encountered an error doing what you asked. Could you try again?",
	"Sorry, something went wrong. Please try your request again.",
}

// BuiltInCannedReplies returns the canned failure messages that are always
// recognized.
func BuiltInCannedReplies() []string {
	return append([]string(nil), builtInCannedReplies...)
}

// CannedReplies recognizes Gemini Web's canned failure messages: the built-in
// ones plus any extra phrases. The zero value recognizes the built-in ones.
type CannedReplies struct {
	normalized map[string]struct{}
}

// NewCannedReplies returns a matcher for the built-in phrases plus extra.
// Blank extra phrases are ignored.
func NewCannedReplies(extra []string) CannedReplies {
	set := make(map[string]struct{}, len(builtInCannedReplies)+len(extra))
	for _, phrase := range append(BuiltInCannedReplies(), extra...) {
		if normalized := NormalizeCannedReply(phrase); normalized != "" {
			set[normalized] = struct{}{}
		}
	}
	return CannedReplies{normalized: set}
}

var builtInMatcher = NewCannedReplies(nil)

func (c CannedReplies) set() map[string]struct{} {
	if c.normalized == nil {
		return builtInMatcher.normalized
	}
	return c.normalized
}

// Is reports whether text is entirely one of the canned failure messages. A
// real answer that merely quotes one is not matched.
func (c CannedReplies) Is(text string) bool {
	_, ok := c.set()[NormalizeCannedReply(text)]
	return ok
}

// CouldBecome reports whether a partial answer is still a prefix of a canned
// failure message. Streaming callers hold such text back until it either
// diverges (a real answer) or completes as a canned reply.
func (c CannedReplies) CouldBecome(partial string) bool {
	normalized := NormalizeCannedReply(partial)
	for reply := range c.set() {
		if strings.HasPrefix(reply, normalized) {
			return true
		}
	}
	return false
}

// IsCannedReply reports whether text is entirely a built-in canned message.
func IsCannedReply(text string) bool { return builtInMatcher.Is(text) }

// CouldBecomeCannedReply reports whether partial is a prefix of a built-in
// canned message.
func CouldBecomeCannedReply(partial string) bool { return builtInMatcher.CouldBecome(partial) }

// NormalizeCannedReply is the form canned phrases are compared in: lower case,
// straight apostrophes and single spaces.
func NormalizeCannedReply(text string) string {
	text = strings.NewReplacer("’", "'", "‘", "'").Replace(text)
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// SuspectedCannedReplyMaxChars bounds the replies SuspectedCannedReply looks at.
const SuspectedCannedReplyMaxChars = 300

// suspectedFailureWords are phrases canned failure messages are built from.
var suspectedFailureWords = []string{
	"error", "try again", "something else", "hard time", "unable to", "can't help", "cannot help",
	"not able to", "went wrong", "can't do that", "not sure how to help",
}

// SuspectedCannedReply flags a short reply worded like a failure message that
// may be a canned phrase not yet configured. It never fails a request: it only
// marks replies worth keeping for analysis, so new canned phrases can be found.
func SuspectedCannedReply(text string) bool {
	if utf8.RuneCountInString(text) > SuspectedCannedReplyMaxChars {
		return false
	}
	normalized := NormalizeCannedReply(text)
	for _, word := range suspectedFailureWords {
		if strings.Contains(normalized, word) {
			return true
		}
	}
	return false
}

// policyRefusalMarkers appear in Gemini Web's templated policy refusals, such
// as "Your request may go against my [guidelines](https://gemini.google/...)".
// Unlike canned failure replies these are not flagged as failed generations.
var policyRefusalMarkers = []string{"gemini.google/policy-guidelines", "my safety filters", "go against my guidelines"}

// IsPolicyRefusal reports whether a short reply is one of Gemini Web's
// templated policy refusals. It is used for analysis only; such replies are
// returned to the client as answers.
func IsPolicyRefusal(text string) bool {
	if utf8.RuneCountInString(text) > 2*SuspectedCannedReplyMaxChars {
		return false
	}
	lower := strings.ToLower(text)
	for _, marker := range policyRefusalMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

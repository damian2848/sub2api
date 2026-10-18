package geminiweb

import (
	"errors"
	"strings"
)

// ErrCannedReply means Gemini Web replaced its answer with one of the fixed
// messages the web app shows when generation fails. The HTTP response is a
// normal 200 with the message as answer text, so callers must check for it.
var ErrCannedReply = errors.New("gemini web replied with a canned error message")

// cannedReplies are the whole-answer fallbacks Gemini Web returns in place of
// a failed generation, normalized by normalizeCannedReply.
var cannedReplies = map[string]struct{}{
	"i seem to be encountering an error. can i try something else for you?":                       {},
	"i'm having a hard time fulfilling your request. can i help you with something else instead?": {},
	"i encountered an error doing what you asked. could you try again?":                           {},
}

// IsCannedReply reports whether text is entirely one of Gemini Web's canned
// failure messages. A real answer that merely quotes one is not matched.
func IsCannedReply(text string) bool {
	_, ok := cannedReplies[normalizeCannedReply(text)]
	return ok
}

func normalizeCannedReply(text string) string {
	text = strings.NewReplacer("’", "'", "‘", "'").Replace(text)
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// CouldBecomeCannedReply reports whether a partial answer is still a prefix of
// a canned failure message. Streaming callers hold such text back until it
// either diverges (a real answer) or completes as a canned reply.
func CouldBecomeCannedReply(partial string) bool {
	normalized := normalizeCannedReply(partial)
	for reply := range cannedReplies {
		if strings.HasPrefix(reply, normalized) {
			return true
		}
	}
	return false
}

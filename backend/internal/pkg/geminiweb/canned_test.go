package geminiweb

import (
	"strings"
	"testing"
)

func TestIsCannedReply(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"I seem to be encountering an error. Can I try something else for you?", true},
		{"  I’m having a hard time fulfilling your request.\nCan I help you with something else instead?  ", true},
		{"I ENCOUNTERED AN ERROR DOING WHAT YOU ASKED. COULD YOU TRY AGAIN?", true},
		{"Sorry, something went wrong. Please try your request again.", true},
		{"  SORRY, something went wrong.\nPlease try your request again.  ", true},
		{"The page says: Sorry, something went wrong. Please try your request again.", false},
		{"背景滚动效果。I seem to be encountering an error. Can I try something else for you?", false},
		{"The page shows: I encountered an error doing what you asked. Could you try again?", false},
		{"I seem to be encountering an error.", false},
		{"", false},
	} {
		if got := IsCannedReply(tc.text); got != tc.want {
			t.Errorf("IsCannedReply(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestCouldBecomeCannedReply(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"", true},
		{"I", true},
		{"I seem to be", true},
		{"I’m having a hard", true},
		{"Sorry, something went wrong.", true},
		{"I seem to be encountering an error. Can I try something else for you?", true},
		{"I will create", false},
		{"我将创建", false},
		{"I seem to be fine", false},
	} {
		if got := CouldBecomeCannedReply(tc.text); got != tc.want {
			t.Errorf("CouldBecomeCannedReply(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestCannedRepliesWithExtraPhrases(t *testing.T) {
	matcher := NewCannedReplies([]string{"  Something went   wrong. Please try later. ", ""})
	if !matcher.Is("something went wrong. please try later.") {
		t.Error("extra phrase not matched")
	}
	if !matcher.Is("I seem to be encountering an error. Can I try something else for you?") {
		t.Error("built-in phrase lost when extras are added")
	}
	if !matcher.CouldBecome("Something went") || matcher.CouldBecome("Something else") {
		t.Error("prefix check does not cover extra phrases")
	}
	if (CannedReplies{}).Is("Something went wrong. Please try later.") {
		t.Error("zero value must only know the built-in phrases")
	}
}

func TestSuspectedCannedReply(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"Sorry, something went wrong. Please try again later.", true},
		{"I'm unable to help with that request right now.", true},
		{"Hello! How can I help you today?", false},
		{"OK", false},
		{strings.Repeat("an error occurred in the build log ", 20), false},
	} {
		if got := SuspectedCannedReply(tc.text); got != tc.want {
			t.Errorf("SuspectedCannedReply(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

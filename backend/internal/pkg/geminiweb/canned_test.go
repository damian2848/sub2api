package geminiweb

import "testing"

func TestIsCannedReply(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"I seem to be encountering an error. Can I try something else for you?", true},
		{"  I’m having a hard time fulfilling your request.\nCan I help you with something else instead?  ", true},
		{"I ENCOUNTERED AN ERROR DOING WHAT YOU ASKED. COULD YOU TRY AGAIN?", true},
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

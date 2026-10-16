package service

import "testing"

func TestWebUsageEstimatesPlainTextAndThinking(t *testing.T) {
	withoutThinking := webUsageWithThinking("hello world", "answer", "")
	usage := webUsageWithThinking("hello world", "answer", "hidden reasoning")
	if usage.InputTokens <= 0 || usage.OutputTokens <= 0 {
		t.Fatalf("usage = %#v, want positive text estimates", usage)
	}
	if usage.OutputTokens <= withoutThinking.OutputTokens {
		t.Fatalf("thinking text was not included: %#v", usage)
	}
}

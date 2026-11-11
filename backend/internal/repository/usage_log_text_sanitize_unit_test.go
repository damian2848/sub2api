//go:build unit

package repository

import (
	"database/sql"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSanitizeUsageLogText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short valid unchanged", "claude-sonnet-4", 100, "claude-sonnet-4"},
		{"exact length unchanged", "abcde", 5, "abcde"},
		{"ascii truncated", "abcdefgh", 5, "abcde"},
		{"multibyte counted as runes", "中文中文中文", 4, "中文中文"},
		{"multibyte within rune limit kept", "中文中文", 4, "中文中文"},
		{"invalid utf8 dropped", "ua\xff\xfe-x", 100, "ua-x"},
		{"nul dropped", "a\x00b", 100, "ab"},
		{"invalid then truncated", "\xff" + strings.Repeat("a", 10), 3, "aaa"},
		{"all invalid becomes empty", "\xff\xfe", 10, ""},
		{"empty", "", 10, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeUsageLogText(tc.in, tc.max)
			require.Equal(t, tc.want, got)
			require.True(t, utf8.ValidString(got))
			require.LessOrEqual(t, utf8.RuneCountInString(got), tc.max)
		})
	}
}

func TestNullUsageLogText(t *testing.T) {
	require.False(t, nullUsageLogText(nil, 10).Valid)
	empty := ""
	require.False(t, nullUsageLogText(&empty, 10).Valid)
	invalid := "\xff"
	require.False(t, nullUsageLogText(&invalid, 10).Valid, "value that sanitizes to empty is NULL")
	v := "hello world"
	got := nullUsageLogText(&v, 5)
	require.True(t, got.Valid)
	require.Equal(t, "hello", got.String)
}

// TestPrepareUsageLogInsert_SanitizesBoundedTextColumns pins every bounded text
// column to its sanitized value so a client-controlled header (e.g. a 600-char
// User-Agent with obs-text bytes) cannot fail the whole batch INSERT.
func TestPrepareUsageLogInsert_SanitizesBoundedTextColumns(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	ptr := func(s string) *string { return &s }

	userAgent := long(600) + "\xff"
	log := &service.UsageLog{
		UserID:                   1,
		APIKeyID:                 2,
		AccountID:                3,
		RequestID:                "req-sanitize",
		Model:                    long(150),
		UpstreamModel:            ptr(long(150)),
		UpstreamResponseModel:    ptr(long(250)),
		UserAgent:                &userAgent,
		IPAddress:                ptr(long(60)),
		ImageSize:                ptr(long(20)),
		ImageInputSize:           ptr(long(40)),
		ImageOutputSize:          ptr(long(40)),
		ImageSizeSource:          ptr(long(20)),
		VideoResolution:          ptr(long(20)),
		ServiceTier:              ptr(long(20)),
		ReasoningEffort:          ptr(long(30)),
		RequestedReasoningEffort: ptr(long(30)),
		InboundEndpoint:          ptr(long(200)),
		UpstreamEndpoint:         ptr(long(200)),
		ModelMappingChain:        ptr(long(600)),
		BillingTier:              ptr(long(60)),
		BillingMode:              ptr(long(30)),
		UpstreamRequestID:        ptr(long(200)),
		SessionID:                ptr(long(300) + "\x00"),
		CreatedAt:                time.Now().UTC(),
	}
	prepared := prepareUsageLogInsert(log)
	require.Len(t, prepared.args, len(usageLogInsertArgTypes))

	require.Equal(t, long(100), prepared.args[4], "model")

	expectNull := map[int]int{
		5:  100, // requested_model (falls back to model)
		6:  100, // upstream_model
		7:  200, // upstream_response_model
		35: 512, // user_agent
		36: 45,  // ip_address
		38: 10,  // image_size
		39: 32,  // image_input_size
		40: 32,  // image_output_size
		41: 16,  // image_size_source
		44: 10,  // video_resolution
		46: 16,  // service_tier
		47: 20,  // reasoning_effort
		48: 20,  // requested_reasoning_effort
		49: 128, // inbound_endpoint
		50: 128, // upstream_endpoint
		54: 500, // model_mapping_chain
		55: 50,  // billing_tier
		56: 20,  // billing_mode
		58: 128, // upstream_request_id
		59: 255, // session_id
	}
	for idx, maxLen := range expectNull {
		require.Equal(t, "text", usageLogInsertArgTypes[idx], "arg %d must be a text column", idx)
		ns, ok := prepared.args[idx].(sql.NullString)
		require.True(t, ok, "arg %d should be sql.NullString, got %T", idx, prepared.args[idx])
		require.True(t, ns.Valid, "arg %d", idx)
		require.Equal(t, long(maxLen), ns.String, "arg %d must be truncated to %d chars", idx, maxLen)
	}

	// request_id participates in the dedup key and must stay untouched.
	require.Equal(t, "req-sanitize", prepared.args[3])
	// The caller's struct is not rewritten.
	require.Equal(t, long(150), log.Model)
	require.Equal(t, userAgent, *log.UserAgent)
}

func TestPrepareUsageLogInsert_NormalTextUnchanged(t *testing.T) {
	ua := "claude-cli/2.1.0 (external, cli)"
	ip := "2001:db8::1"
	prepared := prepareUsageLogInsert(&service.UsageLog{
		UserID:    1,
		APIKeyID:  2,
		AccountID: 3,
		RequestID: "req-normal",
		Model:     "claude-sonnet-4-5",
		UserAgent: &ua,
		IPAddress: &ip,
		CreatedAt: time.Now().UTC(),
	})
	require.Equal(t, "claude-sonnet-4-5", prepared.args[4])
	require.Equal(t, sql.NullString{String: "claude-sonnet-4-5", Valid: true}, prepared.args[5])
	require.Equal(t, sql.NullString{String: ua, Valid: true}, prepared.args[35])
	require.Equal(t, sql.NullString{String: ip, Valid: true}, prepared.args[36])
	require.Equal(t, sql.NullString{}, prepared.args[6], "absent upstream_model stays NULL")
}

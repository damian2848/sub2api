package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func geminiWebTimingAnswerBody(t *testing.T, answer string) string {
	payload := make([]any, 5)
	payload[4] = []any{[]any{"candidate", []any{answer}}}
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	outer, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(b)}})
	require.NoError(t, err)
	return string(outer) + "\n"
}

func TestGeminiWebCompletionTimingLog(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantErr    bool
		wantLevel  zapcore.Level
		wantText   bool
		wantStatus int64
	}{
		{name: "answer", status: http.StatusOK, body: "answer", wantLevel: zap.InfoLevel, wantText: true, wantStatus: 200},
		{name: "upstream error", status: http.StatusInternalServerError, wantErr: true, wantLevel: zap.WarnLevel, wantStatus: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
				body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
				status := http.StatusOK
				if req.Method == http.MethodPost {
					status, body = tc.status, ""
					if tc.body != "" {
						body = geminiWebTimingAnswerBody(t, tc.body)
					}
				}
				return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}}
			svc := &GeminiMessagesCompatService{httpUpstream: upstream}
			account := &Account{ID: 245, Platform: PlatformGemini, Type: AccountTypeGeminiWeb,
				Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"}}
			core, logs := observer.New(zap.InfoLevel)
			ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "req-timing")))
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)

			_, err := svc.ForwardAsChatCompletions(ctx, c, account,
				[]byte(`{"model":"gemini-4-argon","stream":true,"messages":[{"role":"user","content":"你好"}]}`))
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			entries := logs.FilterMessage("gemini_web.completion_timing").All()
			require.Len(t, entries, 1, "one timing line per Gemini Web completion")
			entry := entries[0]
			require.Equal(t, tc.wantLevel, entry.Level)
			fields := entry.ContextMap()
			require.Equal(t, "req-timing", fields["request_id"], "the line must carry the request logger's ID")
			require.Equal(t, int64(245), fields["account_id"])
			require.Equal(t, "gemini-4-argon", fields["model"])
			require.Equal(t, tc.wantStatus, fields["upstream_status"])
			require.Greater(t, fields["prompt_chars"], int64(0))
			require.GreaterOrEqual(t, fields["headers_ms"], int64(0))
			require.GreaterOrEqual(t, fields["total_ms"], fields["headers_ms"])
			require.Equal(t, int64(-1), fields["first_thinking_ms"], "no thought was sent")
			if tc.wantText {
				require.GreaterOrEqual(t, fields["first_text_ms"], fields["first_frame_ms"])
				require.Equal(t, int64(len("answer")), fields["text_chars"])
				require.NotContains(t, fields, "error")
			} else {
				require.Equal(t, int64(-1), fields["first_frame_ms"])
				require.Equal(t, int64(-1), fields["first_text_ms"])
				require.Contains(t, fields["error"], "gemini web upstream")
			}
		})
	}
}

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type geminiWebAccountTestHTTPStub struct{ *geminiWebUsageHTTPStub }

func (s *geminiWebAccountTestHTTPStub) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.do(req)
}

func TestGeminiWebAccountTestCannedReplies(t *testing.T) {
	for _, tc := range []struct {
		name, text, custom string
		wantError          bool
	}{
		{name: "existing built-in", text: geminiWebCannedText, wantError: true},
		{name: "retry request built-in", text: geminiWebRetryRequestCannedText, wantError: true},
		{name: "custom", text: "  TEMPORARILY unavailable.\nPlease come back later. ", custom: "Temporarily unavailable. Please come back later.", wantError: true},
		{name: "normal reply", text: "Hello! How can I help you today?"},
		{name: "quoted failure", text: "The page says: " + geminiWebRetryRequestCannedText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, _ := geminiWebFramesStub(t, tc.text)
			svc := &AccountTestService{httpUpstream: &geminiWebAccountTestHTTPStub{upstream}}
			if tc.custom != "" {
				settings, _ := newGeminiWebCannedSettings()
				_, err := settings.SetGeminiWebCannedReplySettings(context.Background(), []string{tc.custom})
				require.NoError(t, err)
				svc.SetSettingService(settings)
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

			err := svc.testGeminiWebAccountConnection(c, geminiWebTestAccount(), "gemini-3.5-flash-lite", "hi")
			body := recorder.Body.String()
			if tc.wantError {
				require.Error(t, err)
				require.Contains(t, body, `"type":"error"`)
				require.Contains(t, body, GeminiWebCannedReplyClientMessage)
				require.NotContains(t, body, `"type":"content"`)
				require.NotContains(t, body, `"type":"test_complete"`)
			} else {
				require.NoError(t, err)
				require.Contains(t, body, `"type":"content"`)
				require.Contains(t, body, `"success":true`)
				require.NotContains(t, body, `"type":"error"`)
			}
		})
	}
}

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiWebForwardResponseModelAudit(t *testing.T) {
	for _, protocol := range []string{"native", "claude", "chat"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name     string
				ids      []string
				want     string
				conflict bool
			}{
				{"downgraded", []string{"cf41b0e0dd7d53e5"}, "gemini-3.5-flash-lite", false},
				{"matching", []string{"95a4c0abcba24364"}, "gemini-4-argon", false},
				{"missing", []string{""}, "", false},
				{"conflicting", []string{"95a4c0abcba24364", "cf41b0e0dd7d53e5"}, "gemini-3.5-flash-lite", true},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, stream, tc.name), func(t *testing.T) {
					calls := 0
					upstream := &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
						body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
						if strings.Contains(req.URL.Path, "StreamGenerate") {
							calls++
							body = ""
							for _, id := range tc.ids {
								payload := make([]any, 43)
								payload[4] = []any{[]any{"candidate", []any{"OK"}}}
								payload[39] = id
								b, err := json.Marshal(payload)
								require.NoError(t, err)
								outer, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(b)}})
								require.NoError(t, err)
								body += string(outer) + "\n"
							}
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
					}}
					svc := &GeminiMessagesCompatService{httpUpstream: upstream}
					account := &Account{Platform: PlatformGemini, Type: AccountTypeGeminiWeb,
						Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"}}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
					var result *ForwardResult
					var err error
					switch protocol {
					case "native":
						result, err = svc.ForwardNative(context.Background(), c, account, "gemini-4-argon", "generateContent", stream,
							[]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
					case "claude":
						result, err = svc.Forward(context.Background(), c, account, []byte(fmt.Sprintf(
							`{"model":"gemini-4-argon","stream":%t,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, stream)))
					case "chat":
						result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, []byte(fmt.Sprintf(
							`{"model":"gemini-4-argon","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)))
					}
					require.NoError(t, err, "model observation must not turn successful replies into failover")
					require.Equal(t, 1, calls)
					require.Equal(t, http.StatusOK, recorder.Code)
					require.Contains(t, recorder.Body.String(), "OK")
					require.Contains(t, recorder.Body.String(), "gemini-4-argon", "preserve the normal client-facing model alias")
					require.Equal(t, "gemini-4-argon", result.Model)
					require.Equal(t, "gemini-4-argon", result.UpstreamModel)
					require.Equal(t, tc.want, result.UpstreamResponseModel)
					require.Equal(t, tc.conflict, result.UpstreamResponseModelConflict)
					mismatch := upstreamModelMismatch(result.UpstreamModel, result.UpstreamResponseModel)
					if tc.want == "" {
						require.Nil(t, mismatch)
					} else {
						require.NotNil(t, mismatch)
						require.Equal(t, tc.want != result.UpstreamModel, *mismatch)
					}
				})
			}
		}
	}
}

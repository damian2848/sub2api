//go:build unit

package service

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDetectGeminiMalformedFunctionCall(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantOK      bool
		wantContent string
	}{
		{name: "empty", in: "", wantOK: false},
		{name: "normal stop", in: `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`, wantOK: false},
		{
			name:        "malformed with content",
			in:          `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}]}`,
			wantOK:      true,
			wantContent: `{"role":"model","parts":[{"text":"x"}]}`,
		},
		{name: "malformed without content", in: `{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}`, wantOK: true},
		// 标记只出现在文本里（例如模型在回答中提到该词）不能误判
		{name: "marker in text only", in: `{"candidates":[{"content":{"parts":[{"text":"MALFORMED_FUNCTION_CALL"}]},"finishReason":"STOP"}]}`, wantOK: false},
		// 只看 candidates[0]，与原 map 解析语义一致
		{name: "second candidate only", in: `{"candidates":[{"finishReason":"STOP"},{"finishReason":"MALFORMED_FUNCTION_CALL"}]}`, wantOK: false},
		// 原实现 json.Unmarshal 失败即不打日志，无效 JSON 保持不命中
		{name: "invalid json", in: `{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}`, wantOK: false},
		{name: "non-string finishReason", in: `{"candidates":[{"finishReason":{"v":"MALFORMED_FUNCTION_CALL"}}]}`, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content, ok := detectGeminiMalformedFunctionCall([]byte(tc.in))
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantContent, content)
		})
	}
}

// 流里出现 MALFORMED_FUNCTION_CALL chunk 时，转发字节与 usage 不受日志检测影响。
func TestHandleGeminiStreamingResponse_MalformedFunctionCallPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityTestService(&config.Config{
		Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Body: pr, Header: http.Header{}}

	first := `{"candidates":[{"content":{"parts":[{"text":"Hi"}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1}}`
	malformed := `{"candidates":[{"content":{"role":"model","parts":[{"text":"call:x{"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":4}}`
	go func() {
		defer func() { _ = pw.Close() }()
		fmt.Fprintf(pw, "data: {\"response\":%s}\n\n", first)
		fmt.Fprintf(pw, "data: {\"response\":%s}\n\n", malformed)
	}()

	result, err := svc.handleGeminiStreamingResponse(c, resp, time.Now())
	_ = pr.Close()

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 5, result.usage.InputTokens)
	require.Equal(t, 4, result.usage.OutputTokens)
	require.Equal(t, "data: "+first+"\n\ndata: "+malformed+"\n\n", rec.Body.String())
}

func BenchmarkDetectGeminiMalformedFunctionCall_LargeInlineData(b *testing.B) {
	chunk := []byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` +
		strings.Repeat("QUJD", 256*1024) + `"}}]}}],"usageMetadata":{"promptTokenCount":1}}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	for i := 0; i < b.N; i++ {
		if _, ok := detectGeminiMalformedFunctionCall(chunk); ok {
			b.Fatal("unexpected match")
		}
	}
}

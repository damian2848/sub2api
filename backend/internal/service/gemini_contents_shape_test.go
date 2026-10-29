package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const shapeSecret = "SECRET-PROMPT-TEXT"

func TestDescribeGeminiContentsShapeModelEnding(t *testing.T) {
	body := []byte(`{
		"systemInstruction": {"parts": [{"text": "` + shapeSecret + `"}]},
		"tools": [{"functionDeclarations": [{"name": "secret_tool"}]}],
		"contents": [
			{"role": "user", "parts": [{"text": "` + shapeSecret + `"}]},
			{"role": "model", "parts": [{"text": "thinking", "thought": true}, {"functionCall": {"name": "secret_tool", "args": {"q": "` + shapeSecret + `"}}, "thoughtSignature": "sig"}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "secret_tool", "response": {"v": "` + shapeSecret + `"}}}]},
			{"role": "model", "parts": [{"text": "前缀"}]}
		]
	}`)

	shape := describeGeminiContentsShape(body, 4)

	require.Equal(t, geminiContentsShape{
		Contents:          4,
		SystemInstruction: true,
		Tools:             1,
		Tail: []geminiContentShape{
			{Index: 0, Role: "user", Parts: []string{"text(18)"}},
			{Index: 1, Role: "model", Parts: []string{"thought:text(8)", "functionCall"}},
			{Index: 2, Role: "user", Parts: []string{"functionResponse"}},
			{Index: 3, Role: "model", Parts: []string{"text(2)"}},
		},
	}, shape)
	encoded, err := json.Marshal(shape)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), shapeSecret)
	require.NotContains(t, string(encoded), "secret_tool")
}

func TestDescribeGeminiContentsShapeKeepsOnlyTail(t *testing.T) {
	body := []byte(`{"contents": [
		{"role": "user", "parts": [{"text": "a"}]},
		{"role": "model", "parts": [{"text": "b"}]},
		{"role": "user", "parts": [{"text": "c"}]}
	]}`)

	shape := describeGeminiContentsShape(body, 2)

	require.Equal(t, 3, shape.Contents)
	require.Len(t, shape.Tail, 2)
	require.Equal(t, 1, shape.Tail[0].Index)
	require.Equal(t, 2, shape.Tail[1].Index)
}

func TestDescribeGeminiContentsShapeIrregularParts(t *testing.T) {
	body := []byte(`{"contents": [
		{"parts": [{"text": ""}]},
		{"role": "function", "parts": [{}]},
		{"role": "tool", "parts": []},
		{"role": "user", "parts": [{"inline_data": {"mime_type": "image/png", "data": "` + shapeSecret + `"}}, {"fileData": {"mimeType": "application/pdf", "fileUri": "https://example.com/` + shapeSecret + `"}}, {"weird": 1}]},
		{"role": "user"},
		{"role": 7, "parts": "x"},
		"not-an-object"
	]}`)

	shape := describeGeminiContentsShape(body, 10)

	require.Equal(t, []geminiContentShape{
		{Index: 0, Role: "<missing>", Parts: []string{"empty_text"}},
		{Index: 1, Role: "function", Parts: []string{"empty"}},
		{Index: 2, Role: "tool", Parts: []string{}},
		{Index: 3, Role: "user", Parts: []string{"inlineData:image/png", "fileData:application/pdf", "unknown:weird"}},
		{Index: 4, Role: "user", Parts: []string{"<missing>"}},
		{Index: 5, Role: "<not_string>", Parts: []string{"<not_array>"}},
		{Index: 6, Role: "<not_object>", Parts: []string{}},
	}, shape.Tail)
	encoded, err := json.Marshal(shape)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), shapeSecret)
}

func TestDescribeGeminiContentsShapeTruncatesClientStrings(t *testing.T) {
	longRole := strings.Repeat("r", 100)
	body := []byte(`{"contents": [{"role": "` + longRole + `", "parts": [{"` + strings.Repeat("k", 100) + `": 1}]}]}`)

	shape := describeGeminiContentsShape(body, 4)

	require.Equal(t, strings.Repeat("r", geminiShapeRoleMaxLen)+"…", shape.Tail[0].Role)
	require.Equal(t, "unknown:"+strings.Repeat("k", geminiShapeKeyMaxLen)+"…", shape.Tail[0].Parts[0])
}

func TestDescribeGeminiContentsShapeInvalidJSON(t *testing.T) {
	shape := describeGeminiContentsShape([]byte(`{"contents": [`), 4)

	require.Equal(t, "invalid_json", shape.Error)
	require.Empty(t, shape.Tail)
}

func TestIsGeminiModelTurnRejection(t *testing.T) {
	msg := "Requests ending with a model turn are not supported."
	require.True(t, isGeminiModelTurnRejection(400, msg))
	require.False(t, isGeminiModelTurnRejection(429, msg))
	require.False(t, isGeminiModelTurnRejection(400, "The input token count exceeds the maximum number of tokens allowed (1048576)."))
}

func TestLogGeminiModelTurnRejectionWritesShapesWithoutText(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	received := []byte(`{"contents": [{"role": "user", "parts": [{"text": "` + shapeSecret + `"}]}, {"role": "model", "parts": [{"text": "` + shapeSecret + `"}]}]}`)
	forwarded := []byte(`{"systemInstruction": {"parts": [{"text": "identity"}]}, "contents": [{"role": "user", "parts": [{"text": "` + shapeSecret + `"}]}, {"role": "model", "parts": [{"text": "` + shapeSecret + `"}]}]}`)

	logGeminiModelTurnRejection(ctx, &Account{ID: 354}, "gemini-4-argon-medium", "gemini-4-argon-medium", false, received, forwarded)

	entries := logs.FilterMessage("antigravity.gemini_model_turn_rejected").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, int64(354), fields["account_id"])
	require.Equal(t, false, fields["stream"])
	receivedShape, ok := fields["received_shape"].(geminiContentsShape)
	require.True(t, ok)
	require.Equal(t, "model", receivedShape.Tail[len(receivedShape.Tail)-1].Role)
	forwardedShape, ok := fields["forwarded_shape"].(geminiContentsShape)
	require.True(t, ok)
	require.True(t, forwardedShape.SystemInstruction)

	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), shapeSecret)
	require.NotContains(t, string(encoded), "identity")
}

func TestAntigravityGatewayService_ForwardGemini_LogsModelTurnRejectionShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)

	body := []byte(`{"contents": [
		{"role": "user", "parts": [{"text": "` + shapeSecret + `"}]},
		{"role": "model", "parts": [{"text": "` + shapeSecret + `"}]}
	]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/antigravity/v1beta/models/gemini-4-argon-medium:generateContent", bytes.NewReader(body))

	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":400,"message":"Requests ending with a model turn are not supported.","status":"INVALID_ARGUMENT"}}`)),
		}},
	}
	svc := &AntigravityGatewayService{
		settingService: NewSettingService(&antigravitySettingRepoStub{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}),
		tokenProvider:  &AntigravityTokenProvider{},
		httpUpstream:   upstream,
	}
	const model = "gemini-4-argon-medium"
	account := &Account{
		ID:          354,
		Name:        "acc-model-turn",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":  "token",
			"project_id":    "proj",
			"model_mapping": map[string]any{model: model},
		},
	}
	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))

	_, err := svc.ForwardGemini(ctx, c, account, model, "generateContent", false, body, false)

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, writer.Code)
	entries := logs.FilterMessage("antigravity.gemini_model_turn_rejected").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	received, ok := fields["received_shape"].(geminiContentsShape)
	require.True(t, ok)
	forwarded, ok := fields["forwarded_shape"].(geminiContentsShape)
	require.True(t, ok)
	require.Equal(t, []geminiContentShape{
		{Index: 0, Role: "user", Parts: []string{"text(18)"}},
		{Index: 1, Role: "model", Parts: []string{"text(18)"}},
	}, received.Tail)
	require.False(t, received.SystemInstruction)
	require.Equal(t, received.Tail, forwarded.Tail)
	require.True(t, forwarded.SystemInstruction, "identity patch is injected before forwarding")
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), shapeSecret)
}

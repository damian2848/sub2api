package service

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// geminiModelTurnRejectedMarker 是上游拒绝“末轮为 model”的 400 消息片段。
const geminiModelTurnRejectedMarker = "ending with a model turn"

// geminiContentsShapeTail 控制记录 contents 末尾的条数。
const geminiContentsShapeTail = 4

// 客户端可控的字符串（role、未知字段名、mimeType）记录前截断，避免把正文写进日志。
const (
	geminiShapeRoleMaxLen = 32
	geminiShapeKeyMaxLen  = 32
	geminiShapeMimeMaxLen = 64
)

// geminiContentsShape 只描述 Gemini 请求的结构，不包含任何正文、函数名或参数。
type geminiContentsShape struct {
	Contents          int                  `json:"contents"`
	SystemInstruction bool                 `json:"system_instruction"`
	Tools             int                  `json:"tools"`
	Tail              []geminiContentShape `json:"tail"`
	Error             string               `json:"error,omitempty"`
}

type geminiContentShape struct {
	Index int      `json:"index"`
	Role  string   `json:"role"`
	Parts []string `json:"parts"`
}

// 不单独构成内容的 part 修饰字段。
var geminiPartModifierKeys = map[string]bool{
	"thought":           true,
	"thoughtSignature":  true,
	"thought_signature": true,
	"mediaResolution":   true,
	"media_resolution":  true,
	"videoMetadata":     true,
	"video_metadata":    true,
	"partMetadata":      true,
}

// geminiPartContentKeys 映射内容字段到记录名，兼容 snake_case。
var geminiPartContentKeys = map[string]string{
	"text":                  "text",
	"inlineData":            "inlineData",
	"inline_data":           "inlineData",
	"fileData":              "fileData",
	"file_data":             "fileData",
	"functionCall":          "functionCall",
	"function_call":         "functionCall",
	"functionResponse":      "functionResponse",
	"function_response":     "functionResponse",
	"executableCode":        "executableCode",
	"executable_code":       "executableCode",
	"codeExecutionResult":   "codeExecutionResult",
	"code_execution_result": "codeExecutionResult",
}

// describeGeminiContentsShape 提取 contents 末尾若干条的 role 和 part 类型。
func describeGeminiContentsShape(body []byte, tail int) geminiContentsShape {
	var request struct {
		Contents             []json.RawMessage `json:"contents"`
		SystemInstruction    json.RawMessage   `json:"systemInstruction"`
		SystemInstructionAlt json.RawMessage   `json:"system_instruction"`
		Tools                []json.RawMessage `json:"tools"`
	}
	shape := geminiContentsShape{Tail: []geminiContentShape{}}
	if err := json.Unmarshal(body, &request); err != nil {
		shape.Error = "invalid_json"
		return shape
	}
	shape.Contents = len(request.Contents)
	shape.SystemInstruction = hasJSONValue(request.SystemInstruction) || hasJSONValue(request.SystemInstructionAlt)
	shape.Tools = len(request.Tools)

	start := len(request.Contents) - tail
	if start < 0 {
		start = 0
	}
	for index := start; index < len(request.Contents); index++ {
		shape.Tail = append(shape.Tail, describeGeminiContent(index, request.Contents[index]))
	}
	return shape
}

func describeGeminiContent(index int, raw json.RawMessage) geminiContentShape {
	result := geminiContentShape{Index: index, Role: "<missing>", Parts: []string{}}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(raw, &content); err != nil {
		result.Role = "<not_object>"
		return result
	}
	if rawRole, ok := content["role"]; ok {
		var role string
		if err := json.Unmarshal(rawRole, &role); err != nil {
			result.Role = "<not_string>"
		} else {
			result.Role = truncateShapeValue(role, geminiShapeRoleMaxLen)
		}
	}
	rawParts, ok := content["parts"]
	if !ok {
		result.Parts = append(result.Parts, "<missing>")
		return result
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(rawParts, &parts); err != nil {
		result.Parts = append(result.Parts, "<not_array>")
		return result
	}
	for _, part := range parts {
		result.Parts = append(result.Parts, describeGeminiPart(part))
	}
	return result
}

func describeGeminiPart(raw json.RawMessage) string {
	var part map[string]json.RawMessage
	if err := json.Unmarshal(raw, &part); err != nil {
		return "<not_object>"
	}
	thought := false
	if rawThought, ok := part["thought"]; ok {
		_ = json.Unmarshal(rawThought, &thought)
	}
	kinds := make([]string, 0, 1)
	for key, value := range part {
		if geminiPartModifierKeys[key] {
			continue
		}
		name, known := geminiPartContentKeys[key]
		if !known {
			kinds = append(kinds, "unknown:"+truncateShapeValue(key, geminiShapeKeyMaxLen))
			continue
		}
		switch name {
		case "text":
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				kinds = append(kinds, "text<not_string>")
			} else if strings.TrimSpace(text) == "" {
				kinds = append(kinds, "empty_text")
			} else {
				kinds = append(kinds, "text("+strconv.Itoa(utf8.RuneCountInString(text))+")")
			}
		case "inlineData", "fileData":
			var media struct {
				MimeType  string `json:"mimeType"`
				MimeType2 string `json:"mime_type"`
			}
			_ = json.Unmarshal(value, &media)
			mime := media.MimeType
			if mime == "" {
				mime = media.MimeType2
			}
			kinds = append(kinds, name+":"+truncateShapeValue(mime, geminiShapeMimeMaxLen))
		default:
			kinds = append(kinds, name)
		}
	}
	if len(kinds) == 0 {
		kinds = append(kinds, "empty")
	}
	// map 遍历无序；排序保证同一结构的日志一致。
	sort.Strings(kinds)
	description := strings.Join(kinds, "+")
	if thought {
		description = "thought:" + description
	}
	return description
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func truncateShapeValue(value string, maxLen int) string {
	if utf8.RuneCountInString(value) <= maxLen {
		return value
	}
	return string([]rune(value)[:maxLen]) + "…"
}

// isGeminiModelTurnRejection 判断上游 400 是否为“末轮为 model”拒绝。
func isGeminiModelTurnRejection(statusCode int, upstreamMsg string) bool {
	return statusCode == 400 && strings.Contains(strings.ToLower(upstreamMsg), geminiModelTurnRejectedMarker)
}

// logGeminiModelTurnRejection 记录被拒请求在 Sub2API 改写前后的结构，用于定位末轮角色来源。
// received 是 ForwardGemini 收到的请求体，forwarded 是包装 v1internal 前的最终请求体。
func logGeminiModelTurnRejection(ctx context.Context, account *Account, requestedModel, mappedModel string, stream bool, received, forwarded []byte) {
	fields := []zap.Field{
		zap.String("component", "service.antigravity_gateway"),
		zap.String("requested_model", requestedModel),
		zap.String("mapped_model", mappedModel),
		zap.Bool("stream", stream),
		zap.Any("received_shape", describeGeminiContentsShape(received, geminiContentsShapeTail)),
		zap.Any("forwarded_shape", describeGeminiContentsShape(forwarded, geminiContentsShapeTail)),
	}
	if account != nil {
		fields = append(fields, zap.Int64("account_id", account.ID))
	}
	logger.FromContext(ctx).Warn("antigravity.gemini_model_turn_rejected", fields...)
}

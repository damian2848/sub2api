package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/gin-gonic/gin"
)

// Gemini Web has no function-calling API. Tools are described in the prompt,
// the model answers with <tool_call> blocks, and the gateway turns those
// blocks back into native functionCall parts. Every protocol adapter goes
// through the Gemini generateContent shape, so this file only deals with it.

const (
	geminiWebToolCallOpen  = "<tool_call>"
	geminiWebToolCallClose = "</tool_call>"
	// geminiWebKeepAliveLine is written into the internal Gemini SSE pipe while
	// a tool call is being held back. The protocol converters turn it into a
	// client keepalive; real Google streams never contain it.
	geminiWebKeepAliveLine = ": sub2api-gemini-web-keepalive"
)

var geminiWebKeepAliveInterval = 5 * time.Second

// geminiWebFirstTokenPingDelay is how long a compat stream stays silent while
// Gemini Web has produced no answer yet. After it, the response is committed
// and the client gets SSE comment pings so its first-byte timeout does not fire
// while a Pro model is thinking. Pings are not content: until the converter
// starts, the request can still switch accounts on the same response.
var geminiWebFirstTokenPingDelay = 5 * time.Second

// geminiWebPingBytesKey counts the SSE comment ping bytes written to the client
// before any content. The handlers subtract them (GeminiWebPingAdjustedWrittenSize)
// so an attempt that only sent pings can still switch accounts.
const geminiWebPingBytesKey = "gemini_web_ping_bytes"

func addGeminiWebPingBytes(c *gin.Context, n int) {
	current, _ := c.Get(geminiWebPingBytesKey)
	total, _ := current.(int)
	c.Set(geminiWebPingBytesKey, total+n)
}

// GeminiWebPingAdjustedWrittenSize is the client writer size without the
// first-token pings of Gemini Web compat streams. Compare it before and after a
// forward attempt: equal values mean the client has received no content, so
// failing over to another account cannot corrupt the stream. Like gin's own
// Size, it is -1 while nothing but pings has been written, and it is exactly
// c.Writer.Size() when no pings were sent.
func GeminiWebPingAdjustedWrittenSize(c *gin.Context) int {
	if c == nil || c.Writer == nil {
		return -1
	}
	size := c.Writer.Size()
	current, _ := c.Get(geminiWebPingBytesKey)
	pings, _ := current.(int)
	if pings <= 0 {
		return size
	}
	if real := size - pings; real > 0 {
		return real
	}
	return -1
}

type geminiWebTool struct {
	Name        string
	Description string
	Parameters  any
}

type geminiWebToolCall struct {
	Name string
	Args map[string]any
}

type geminiWebPromptPlan struct {
	Prompt string
	Tools  []geminiWebTool
	// RequireTool mirrors functionCallingConfig mode ANY.
	RequireTool bool
}

func (p geminiWebPromptPlan) toolsEnabled() bool { return len(p.Tools) > 0 }

func (p geminiWebPromptPlan) allows(name string) bool {
	for _, tool := range p.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func geminiWebPrompt(body []byte) (string, error) {
	plan, err := geminiWebBuildPrompt(body)
	return plan.Prompt, err
}

// geminiWebBuildPrompt flattens a generateContent body into one browser
// prompt. Plain conversations keep the original "role: text" format; requests
// that declare tools or carry tool history use a tagged transcript, which
// keeps calls and results attributable to the right turn.
func geminiWebBuildPrompt(body []byte) (geminiWebPromptPlan, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return geminiWebPromptPlan{}, err
	}
	contents, _ := root["contents"].([]any)
	if geminiWebHasMedia(root["systemInstruction"]) || geminiWebHasMedia(contents) {
		return geminiWebPromptPlan{}, errors.New("gemini web provider currently supports text input only")
	}
	tools, requireTool := geminiWebDeclaredTools(root)
	if len(tools) == 0 && !geminiWebHasToolHistory(contents) {
		prompt, err := geminiWebLegacyPrompt(root, contents)
		return geminiWebPromptPlan{Prompt: prompt}, err
	}

	var b strings.Builder
	if system := geminiWebPartsText(root["systemInstruction"]); system != "" {
		_, _ = b.WriteString("<system>\n" + system + "\n</system>\n\n")
	}
	if len(tools) > 0 {
		writeGeminiWebToolInstructions(&b, tools, requireTool)
	} else {
		_, _ = b.WriteString("Tool calling is not available for this reply. Answer in plain text, using the tool results in the conversation.\n\n")
	}
	_, _ = b.WriteString("<conversation>\n")
	turns := 0
	for _, raw := range contents {
		content, _ := raw.(map[string]any)
		text := geminiWebTranscriptParts(content["parts"])
		if text == "" {
			continue
		}
		tag := "user"
		if role, _ := content["role"].(string); role == "model" {
			tag = "assistant"
		}
		_, _ = b.WriteString("<" + tag + ">\n" + text + "\n</" + tag + ">\n")
		turns++
	}
	if turns == 0 {
		return geminiWebPromptPlan{}, errors.New("request contains no text parts")
	}
	_, _ = b.WriteString("</conversation>\n\nContinue the conversation as the assistant. Reply with only your next message.")
	return geminiWebPromptPlan{Prompt: b.String(), Tools: tools, RequireTool: requireTool && len(tools) > 0}, nil
}

func geminiWebLegacyPrompt(root map[string]any, contents []any) (string, error) {
	var parts []string
	appendParts := func(raw any) {
		items, _ := raw.([]any)
		for _, item := range items {
			m, _ := item.(map[string]any)
			if text, ok := m["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
	}
	if system, ok := root["systemInstruction"].(map[string]any); ok {
		appendParts(system["parts"])
	}
	for _, raw := range contents {
		m, _ := raw.(map[string]any)
		role, _ := m["role"].(string)
		before := len(parts)
		appendParts(m["parts"])
		if len(parts) > before && role != "" {
			parts[before] = role + ": " + parts[before]
		}
	}
	if len(parts) == 0 {
		return "", errors.New("request contains no text parts")
	}
	return strings.Join(parts, "\n"), nil
}

func geminiWebHasMedia(raw any) bool {
	switch v := raw.(type) {
	case map[string]any:
		if _, ok := v["inlineData"]; ok {
			return true
		}
		if _, ok := v["fileData"]; ok {
			return true
		}
		return geminiWebHasMedia(v["parts"])
	case []any:
		for _, item := range v {
			if geminiWebHasMedia(item) {
				return true
			}
		}
	}
	return false
}

func geminiWebHasToolHistory(contents []any) bool {
	for _, raw := range contents {
		content, _ := raw.(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if part["functionCall"] != nil || part["functionResponse"] != nil {
				return true
			}
		}
	}
	return false
}

// geminiWebDeclaredTools returns the tools the model may call in this reply,
// after applying toolConfig (mode NONE hides them, allowedFunctionNames
// narrows them).
func geminiWebDeclaredTools(root map[string]any) ([]geminiWebTool, bool) {
	mode, allowed := "", map[string]bool{}
	if config, ok := root["toolConfig"].(map[string]any); ok {
		calling, _ := config["functionCallingConfig"].(map[string]any)
		mode, _ = calling["mode"].(string)
		names, _ := calling["allowedFunctionNames"].([]any)
		for _, name := range names {
			if s, ok := name.(string); ok && s != "" {
				allowed[s] = true
			}
		}
	}
	mode = strings.ToUpper(strings.TrimSpace(mode))
	if mode == "NONE" {
		return nil, false
	}
	var tools []geminiWebTool
	seen := map[string]bool{}
	items, _ := root["tools"].([]any)
	for _, item := range items {
		group, _ := item.(map[string]any)
		decls, ok := group["functionDeclarations"].([]any)
		if !ok {
			decls, _ = group["function_declarations"].([]any)
		}
		for _, raw := range decls {
			decl, _ := raw.(map[string]any)
			name, _ := decl["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" || seen[name] || (len(allowed) > 0 && !allowed[name]) {
				continue
			}
			seen[name] = true
			desc, _ := decl["description"].(string)
			params := decl["parametersJsonSchema"]
			if params == nil {
				params = decl["parameters"]
			}
			tools = append(tools, geminiWebTool{Name: name, Description: strings.TrimSpace(desc), Parameters: params})
		}
	}
	return tools, mode == "ANY"
}

func writeGeminiWebToolInstructions(b *strings.Builder, tools []geminiWebTool, requireTool bool) {
	_, _ = b.WriteString("<tools>\nYou can call the tools below. Each tool's parameters are described by a JSON Schema. These tools are the only way to reach the user's environment and live data: use them instead of your own built-in tools, search or cards.\n\n")
	for _, tool := range tools {
		_, _ = b.WriteString("- name: " + tool.Name + "\n")
		if tool.Description != "" {
			_, _ = b.WriteString("  description: " + strings.ReplaceAll(tool.Description, "\n", "\n    ") + "\n")
		}
		params := tool.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		schema, _ := json.Marshal(params)
		_, _ = b.WriteString("  parameters: " + string(schema) + "\n")
	}
	_, _ = b.WriteString("\nTo call a tool, write a block in exactly this format: the tags on their own lines, the body one JSON object.\n")
	_, _ = b.WriteString(geminiWebToolCallOpen + "\n{\"name\": \"<tool name>\", \"arguments\": {<arguments object>}}\n" + geminiWebToolCallClose + "\n")
	_, _ = b.WriteString("- Write several blocks to call several tools at once. After the last block stop: the results come back in the next message.\n")
	_, _ = b.WriteString("- Results of earlier calls appear as <tool_result> blocks in the conversation. Use them; do not call a tool again for a result you already have, and never invent results.\n")
	_, _ = b.WriteString("- Do not wrap the blocks in code fences. If no tool is needed, answer normally.\n")
	if requireTool {
		_, _ = b.WriteString("- This reply must call at least one tool.\n")
	}
	_, _ = b.WriteString("</tools>\n\n")
}

func geminiWebPartsText(raw any) string {
	container, _ := raw.(map[string]any)
	parts, _ := container["parts"].([]any)
	var texts []string
	for _, p := range parts {
		part, _ := p.(map[string]any)
		if text, ok := part["text"].(string); ok && strings.TrimSpace(text) != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func geminiWebTranscriptParts(raw any) string {
	parts, _ := raw.([]any)
	var out []string
	for _, p := range parts {
		part, _ := p.(map[string]any)
		if thought, _ := part["thought"].(bool); thought {
			continue
		}
		if text, ok := part["text"].(string); ok {
			if strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
			continue
		}
		if call, ok := part["functionCall"].(map[string]any); ok {
			name, _ := call["name"].(string)
			args := call["args"]
			if args == nil {
				args = map[string]any{}
			}
			encoded, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
			out = append(out, geminiWebToolCallOpen+"\n"+string(encoded)+"\n"+geminiWebToolCallClose)
			continue
		}
		if response, ok := part["functionResponse"].(map[string]any); ok {
			name, _ := response["name"].(string)
			out = append(out, "<tool_result name=\""+name+"\">\n"+geminiWebToolResultText(response["response"])+"\n</tool_result>")
		}
	}
	return strings.Join(out, "\n")
}

// geminiWebToolResultText unwraps the common single-field result envelopes
// ({"content": ...}, {"output": ...}) so the model reads the tool output as
// written instead of an escaped JSON string.
func geminiWebToolResultText(raw any) string {
	if m, ok := raw.(map[string]any); ok && len(m) == 1 {
		for key, value := range m {
			if s, ok := value.(string); ok {
				if key == "error" {
					return "Error: " + s
				}
				return s
			}
		}
	}
	if s, ok := raw.(string); ok {
		return s
	}
	encoded, _ := json.Marshal(raw)
	return string(encoded)
}

// geminiWebExtractToolCalls splits a final answer into its text and the
// <tool_call> blocks naming offered tools. Blocks that do not parse, or that
// name an unknown tool, stay in the text so nothing the model wrote is lost.
func geminiWebExtractToolCalls(text string, plan geminiWebPromptPlan) (string, []geminiWebToolCall) {
	if !plan.toolsEnabled() || !strings.Contains(text, geminiWebToolCallOpen) {
		return text, nil
	}
	var clean strings.Builder
	var calls []geminiWebToolCall
	rest := text
	for {
		start := strings.Index(rest, geminiWebToolCallOpen)
		if start < 0 {
			_, _ = clean.WriteString(rest)
			break
		}
		_, _ = clean.WriteString(rest[:start])
		after := rest[start+len(geminiWebToolCallOpen):]
		body, remainder := after, ""
		if end := strings.Index(after, geminiWebToolCallClose); end >= 0 {
			body, remainder = after[:end], after[end+len(geminiWebToolCallClose):]
		}
		if call, ok := parseGeminiWebToolCall(body, plan); ok {
			calls = append(calls, call)
		} else {
			_, _ = clean.WriteString(rest[start : len(rest)-len(remainder)])
		}
		rest = remainder
		if rest == "" {
			break
		}
	}
	// Only trim the end: the start may already have been streamed.
	return strings.TrimRight(clean.String(), " \t\r\n"), calls
}

func parseGeminiWebToolCall(body string, plan geminiWebPromptPlan) (geminiWebToolCall, bool) {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "```") {
		body = strings.TrimPrefix(body, "```")
		if newline := strings.IndexByte(body, '\n'); newline >= 0 {
			body = body[newline+1:]
		}
		body = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), "```"))
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return geminiWebToolCall{}, false
	}
	name, _ := raw["name"].(string)
	name = strings.TrimSpace(name)
	if !plan.allows(name) {
		return geminiWebToolCall{}, false
	}
	args := map[string]any{}
	for _, key := range []string{"arguments", "args", "parameters", "input"} {
		switch v := raw[key].(type) {
		case map[string]any:
			args = v
		case string:
			var decoded map[string]any
			if json.Unmarshal([]byte(v), &decoded) != nil {
				continue
			}
			args = decoded
		default:
			continue
		}
		break
	}
	return geminiWebToolCall{Name: name, Args: args}, true
}

// geminiWebStreamableText is the prefix of a cumulative snapshot that can be
// sent before the answer is complete: nothing from the first tool call on,
// no trailing characters that could still become "<tool_call>", and no
// trailing whitespace, which the final answer trims before a tool call.
func geminiWebStreamableText(text string, toolsEnabled bool) string {
	if !toolsEnabled {
		return text
	}
	if i := strings.Index(text, geminiWebToolCallOpen); i >= 0 {
		text = text[:i]
	} else {
		for n := len(geminiWebToolCallOpen) - 1; n > 0; n-- {
			if strings.HasSuffix(text, geminiWebToolCallOpen[:n]) {
				text = text[:len(text)-n]
				break
			}
		}
	}
	return strings.TrimRight(text, " \t\r\n")
}

func geminiWebFunctionCallParts(calls []geminiWebToolCall) []any {
	parts := make([]any, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, map[string]any{"functionCall": map[string]any{"name": call.Name, "args": call.Args}})
	}
	return parts
}

// geminiWebGeminiResponse builds a non-streaming generateContent response.
// Thoughts are left out: the Claude/OpenAI converters would render them as
// answer text.
func geminiWebGeminiResponse(text string, calls []geminiWebToolCall, usage ClaudeUsage, responseModel string) ([]byte, map[string]any) {
	parts := []any{}
	if text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	parts = append(parts, geminiWebFunctionCallParts(calls)...)
	resp := map[string]any{
		"candidates":    []any{map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "finishReason": "STOP", "index": 0}},
		"usageMetadata": geminiWebUsageMetadata(usage),
	}
	if responseModel != "" {
		resp["modelVersion"] = responseModel
	}
	raw, _ := json.Marshal(resp)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return raw, decoded
}

// geminiWebClaudeToolChoice copies Claude's tool_choice into the converted
// Gemini body. The shared Claude→Gemini converter does not carry it, and the
// Web prompt needs it to hide or require tools.
func geminiWebClaudeToolChoice(geminiBody, claudeBody []byte) []byte {
	var claude struct {
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if json.Unmarshal(claudeBody, &claude) != nil || len(claude.ToolChoice) == 0 {
		return geminiBody
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(claude.ToolChoice, &choice) != nil {
		return geminiBody
	}
	config := map[string]any{}
	switch choice.Type {
	case "none":
		config["mode"] = "NONE"
	case "any":
		config["mode"] = "ANY"
	case "tool":
		config["mode"] = "ANY"
		if choice.Name != "" {
			config["allowedFunctionNames"] = []any{choice.Name}
		}
	default:
		return geminiBody
	}
	var root map[string]any
	if json.Unmarshal(geminiBody, &root) != nil {
		return geminiBody
	}
	root["toolConfig"] = map[string]any{"functionCallingConfig": config}
	out, err := json.Marshal(root)
	if err != nil {
		return geminiBody
	}
	return out
}

func geminiWebClaudeThinkingEnabled(claudeBody []byte) bool {
	var req struct {
		Thinking struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if json.Unmarshal(claudeBody, &req) != nil {
		return false
	}
	return req.Thinking.Type == "enabled" || req.Thinking.Type == "adaptive"
}

var (
	errGeminiWebConverterStopped = errors.New("gemini web stream converter stopped")
	errGeminiWebStreamFinished   = errors.New("gemini web stream already finished")
)

// geminiWebPipeStream feeds Gemini Web snapshots to the shared native Gemini
// stream converters (Claude Messages, Chat Completions) through an in-memory
// SSE pipe. The converter only starts on the first frame with content, so a
// request that fails before any output can still switch accounts. If no
// content arrives within geminiWebFirstTokenPingDelay, pingUntilFirstToken
// commits the response and sends SSE comment pings instead.
type geminiWebPipeStream struct {
	plan           geminiWebPromptPlan
	includeThought bool
	handle         func(*http.Response) (*geminiStreamResult, error)
	cancel         func()

	mu           sync.Mutex
	pr           *io.PipeReader
	pw           *io.PipeWriter
	started      bool
	finished     bool
	committed    bool
	writeErr     error
	lastWrite    time.Time
	sentText     string
	sentThinking string
	released     bool
	canned       geminiweb.CannedReplies
	done         chan struct{}
	result       *geminiStreamResult
	handleErr    error
	stopKeep     chan struct{}
	stopOnce     sync.Once
}

func newGeminiWebPipeStream(plan geminiWebPromptPlan, includeThought bool, cancel func(), handle func(*http.Response) (*geminiStreamResult, error)) *geminiWebPipeStream {
	pr, pw := io.Pipe()
	return &geminiWebPipeStream{plan: plan, includeThought: includeThought, handle: handle, cancel: cancel,
		pr: pr, pw: pw, done: make(chan struct{}), stopKeep: make(chan struct{})}
}

func (p *geminiWebPipeStream) startLocked() {
	if p.started {
		return
	}
	p.started = true
	go func() {
		defer close(p.done)
		p.result, p.handleErr = p.handle(&http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: p.pr})
		// A converter that stops early (client gone) must unblock the producer.
		_ = p.pr.CloseWithError(errGeminiWebConverterStopped)
	}()
	go p.keepAlive(geminiWebKeepAliveInterval)
}

func (p *geminiWebPipeStream) keepAlive(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.mu.Lock()
			if !p.finished && p.writeErr == nil && time.Since(p.lastWrite) >= interval {
				_ = p.writeLocked([]byte(geminiWebKeepAliveLine + "\n\n"))
			}
			p.mu.Unlock()
		case <-p.stopKeep:
			return
		case <-p.done:
			return
		}
	}
}

// pingUntilFirstToken waits geminiWebFirstTokenPingDelay for the first
// content frame. If none has arrived by then, it commits the client response
// and writes an SSE comment ping every geminiWebKeepAliveInterval until the
// converter starts or the stream ends. Comments are ignored by Anthropic and
// OpenAI SSE clients, so they are safe before message_start / the first chunk.
func (p *geminiWebPipeStream) pingUntilFirstToken(c *gin.Context) {
	delay, interval := geminiWebFirstTokenPingDelay, geminiWebKeepAliveInterval
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-p.stopKeep:
			return
		case <-c.Request.Context().Done():
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for p.pingBeforeFirstToken(c) {
			select {
			case <-ticker.C:
			case <-p.stopKeep:
				return
			case <-c.Request.Context().Done():
				return
			}
		}
	}()
}

// pingBeforeFirstToken writes one comment ping straight to the client. It
// holds p.mu, and the converter only starts under p.mu, so the two never write
// to the client at the same time. It reports whether pinging should go on.
func (p *geminiWebPipeStream) pingBeforeFirstToken(c *gin.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.finished || p.writeErr != nil {
		return false
	}
	// An earlier attempt that failed over after its own pings has already
	// committed the response; keep writing on it.
	if !p.committed && !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
		MarkResponseCommitted(c)
	}
	p.committed = true
	n, err := c.Writer.Write([]byte(": ping\n\n"))
	addGeminiWebPingBytes(c, n)
	if err != nil {
		p.writeErr = err
		if p.cancel != nil {
			p.cancel()
		}
		return false
	}
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
	return true
}

func (p *geminiWebPipeStream) writeLocked(data []byte) error {
	if p.writeErr != nil {
		return p.writeErr
	}
	if _, err := p.pw.Write(data); err != nil {
		p.writeErr = err
		if p.cancel != nil {
			p.cancel()
		}
		return err
	}
	p.lastWrite = time.Now()
	return nil
}

func (p *geminiWebPipeStream) chunkLocked(text, thinking string, calls []geminiWebToolCall, final bool, usage ClaudeUsage, responseModel string) []byte {
	parts := []any{}
	if p.includeThought && thinking != "" && thinking != p.sentThinking {
		parts = append(parts, map[string]any{"text": thinking, "thought": true})
		p.sentThinking = thinking
	}
	// Text parts are cumulative: the converters derive deltas themselves.
	if text != "" && text != p.sentText {
		parts = append(parts, map[string]any{"text": text})
		p.sentText = text
	}
	parts = append(parts, geminiWebFunctionCallParts(calls)...)
	if len(parts) == 0 && !final {
		return nil
	}
	candidate := map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "index": 0}
	payload := map[string]any{"candidates": []any{candidate}}
	if final {
		candidate["finishReason"] = "STOP"
		payload["usageMetadata"] = geminiWebUsageMetadata(usage)
	}
	if responseModel != "" {
		payload["modelVersion"] = responseModel
	}
	encoded, _ := json.Marshal(payload)
	return []byte("data: " + string(encoded) + "\n\n")
}

func (p *geminiWebPipeStream) OnResult(result geminiweb.Result) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		if p.writeErr != nil {
			return p.writeErr
		}
		return errGeminiWebStreamFinished
	}
	// With tools, answer text is held until the reply is complete: Gemini Web
	// can replace a half-written answer with a canned failure message, and
	// only an unsent answer can still fail over to another account.
	// Without tools, text streams as it arrives, except while it could still
	// be the start of a canned failure message.
	text := ""
	if !p.plan.toolsEnabled() {
		text, p.released = geminiWebHoldPossibleCannedReply(geminiWebStreamableText(result.Text, false), p.sentText, p.released, p.canned)
	}
	chunk := p.chunkLocked(text, result.Thinking, nil, false, ClaudeUsage{}, result.ResponseModel)
	if chunk == nil {
		return nil
	}
	p.startLocked()
	return p.writeLocked(chunk)
}

// Finish writes the final frame (remaining text, tool calls, usage) and waits
// for the converter to end the client stream.
func (p *geminiWebPipeStream) Finish(result geminiweb.Result, usage ClaudeUsage) (*geminiStreamResult, error) {
	p.stopOnce.Do(func() { close(p.stopKeep) })
	p.mu.Lock()
	text, calls := geminiWebExtractToolCalls(result.Text, p.plan)
	chunk := p.chunkLocked(text, result.Thinking, calls, true, usage, result.ResponseModel)
	p.startLocked()
	err := p.writeLocked(chunk)
	p.finished = true
	_ = p.pw.Close()
	p.mu.Unlock()
	<-p.done
	if err != nil && !errors.Is(err, errGeminiWebConverterStopped) {
		return p.result, err
	}
	return p.result, p.handleErr
}

// Abort ends the stream after an upstream error. It reports whether content
// had reached the converter. First-token pings do not count: the caller checks
// GeminiWebPingAdjustedWrittenSize to decide whether it may still fail over.
func (p *geminiWebPipeStream) Abort(cause error) bool {
	p.stopOnce.Do(func() { close(p.stopKeep) })
	p.mu.Lock()
	p.finished = true
	started := p.started
	_ = p.pw.CloseWithError(cause)
	p.mu.Unlock()
	if started {
		<-p.done
	}
	return started
}

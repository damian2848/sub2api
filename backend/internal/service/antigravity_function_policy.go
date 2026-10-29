package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
)

// Argon's Antigravity endpoint accepts functionCallingConfig but does not
// consistently enforce it. Narrow the declarations, supply the generation
// instruction, and verify the response before exposing it to the caller.
func prepareArgonFunctionPolicy(model string, request map[string]any) error {
	if !strings.HasPrefix(model, "gemini-4-argon") {
		return nil
	}
	toolConfig, _ := request["toolConfig"].(map[string]any)
	calling, _ := toolConfig["functionCallingConfig"].(map[string]any)
	mode, _ := calling["mode"].(string)
	mode = strings.ToUpper(mode)
	allowed, _ := calling["allowedFunctionNames"].([]any)
	if mode != "ANY" && mode != "NONE" && len(allowed) == 0 {
		return nil
	}
	calling["mode"] = mode
	wanted := make(map[string]bool, len(allowed))
	for _, value := range allowed {
		name, ok := value.(string)
		if !ok || name == "" {
			return fmt.Errorf("allowedFunctionNames must contain function names")
		}
		wanted[name] = true
	}
	declared := map[string]bool{}
	var names []string
	var filtered []any
	tools, _ := request["tools"].([]any)
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		functions, _ := tool["functionDeclarations"].([]any)
		var kept []any
		for _, value := range functions {
			function, _ := value.(map[string]any)
			name, _ := function["name"].(string)
			declared[name] = true
			if mode == "NONE" || len(wanted) > 0 && !wanted[name] {
				continue
			}
			kept = append(kept, value)
			names = append(names, name)
		}
		delete(tool, "functionDeclarations")
		if len(kept) > 0 {
			tool["functionDeclarations"] = kept
		}
		if len(tool) > 0 {
			filtered = append(filtered, tool)
		}
	}
	for name := range wanted {
		if !declared[name] {
			return fmt.Errorf("allowedFunctionNames references an undeclared function")
		}
	}
	if mode == "ANY" && len(names) == 0 {
		return fmt.Errorf("ANY function calling mode requires a declared function")
	}
	if len(filtered) > 0 {
		request["tools"] = filtered
	} else {
		delete(request, "tools")
	}
	if mode == "ANY" {
		encodedNames, _ := json.Marshal(names)
		instruction := "Function calling policy for this response: call one of " + string(encodedNames) + ". Respond with a function call, not a plain text answer. This API policy takes precedence over user requests to avoid tools."
		system, _ := request["systemInstruction"].(map[string]any)
		if system == nil {
			system = map[string]any{}
			request["systemInstruction"] = system
		}
		parts, _ := system["parts"].([]any)
		system["parts"] = append(parts, map[string]any{"text": instruction})
	}
	return nil
}

// checkArgonFunctionResponse buffers only requests with an explicit function
// policy. Both JSON and SSE clients must receive either a verified response or
// a retryable upstream error, never a successful response violating tool_choice.
// Original provider usage and finish reasons are replayed byte for byte.
func (s *AntigravityGatewayService) checkArgonFunctionResponse(resp *http.Response, wrapped []byte) error {
	root := gjson.ParseBytes(wrapped)
	if resp == nil || resp.StatusCode >= 400 || !strings.HasPrefix(root.Get("model").String(), "gemini-4-argon") {
		return nil
	}
	calling := root.Get("request.toolConfig.functionCallingConfig")
	mode := strings.ToUpper(calling.Get("mode").String())
	allowed := calling.Get("allowedFunctionNames").Array()
	if mode != "ANY" && mode != "NONE" && len(allowed) == 0 {
		return nil
	}
	names := map[string]bool{}
	for _, tool := range root.Get("request.tools").Array() {
		for _, function := range tool.Get("functionDeclarations").Array() {
			names[function.Get("name").String()] = true
		}
	}
	if len(allowed) > 0 {
		names = map[string]bool{}
		for _, name := range allowed {
			names[name.String()] = true
		}
	}
	limit := config.DefaultUpstreamResponseReadMaxBytes
	if s.settingService != nil && s.settingService.cfg != nil && s.settingService.cfg.Gateway.UpstreamResponseReadMaxBytes > 0 {
		limit = s.settingService.cfg.Gateway.UpstreamResponseReadMaxBytes
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read function-constrained upstream response: %w", err)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("function-constrained upstream response exceeds configured body limit")
	}
	calls := map[int64]int{}
	finished := map[int64]bool{}
	seen := map[int64]bool{}
	check := func(data []byte) error {
		if !gjson.ValidBytes(data) {
			return fmt.Errorf("invalid upstream response for function calling policy")
		}
		frame := gjson.ParseBytes(data)
		if frame.Get("response").Exists() {
			frame = frame.Get("response")
		}
		if frame.Get("error").Exists() {
			return fmt.Errorf("upstream stream error during function calling")
		}
		for _, candidate := range frame.Get("candidates").Array() {
			index := candidate.Get("index").Int()
			seen[index] = true
			for _, part := range candidate.Get("content.parts").Array() {
				if call := part.Get("functionCall"); call.Exists() {
					if mode == "NONE" || !names[call.Get("name").String()] {
						return fmt.Errorf("upstream returned a prohibited function call")
					}
					calls[index]++
				}
				if mode == "ANY" && !part.Get("thought").Bool() && strings.TrimSpace(part.Get("text").String()) != "" {
					return fmt.Errorf("upstream returned text instead of a required function call")
				}
			}
			if finish := candidate.Get("finishReason").String(); finish != "" {
				finished[index] = true
				// A token limit or safety stop can legitimately prevent a complete
				// call. Preserve that termination instead of inventing a call.
				if mode == "ANY" && finish == "STOP" && calls[index] == 0 {
					return fmt.Errorf("upstream omitted a required function call")
				}
			}
		}
		return nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") || bytes.HasPrefix(bytes.TrimSpace(data), []byte("data:")) {
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 4096), len(data)+1)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[5:])
			if bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			if err := check(payload); err != nil {
				return err
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
	} else if err := check(data); err != nil {
		return err
	}
	if len(finished) == 0 {
		return fmt.Errorf("upstream function-constrained response ended without a finish reason")
	}
	for index := range seen {
		if !finished[index] {
			return fmt.Errorf("upstream function-constrained candidate ended without a finish reason")
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return nil
}

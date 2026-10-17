package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/geminiweb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const geminiWebToolsBody = `{
  "systemInstruction": {"parts": [{"text": "Be brief."}]},
  "tools": [{"functionDeclarations": [
    {"name": "get_weather", "description": "Weather for a city.", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}},
    {"name": "read_file", "description": "Read a file.\nLine two.", "parametersJsonSchema": {"type": "object", "properties": {"path": {"type": "string"}}}}
  ]}],
  "contents": [
    {"role": "user", "parts": [{"text": "Weather in Paris?"}]},
    {"role": "model", "parts": [{"text": "thinking...", "thought": true}, {"functionCall": {"name": "get_weather", "args": {"city": "Paris"}}}]},
    {"role": "user", "parts": [{"functionResponse": {"name": "get_weather", "response": {"content": "18C, rain"}}}]},
    {"role": "user", "parts": [{"text": "And now?"}]}
  ]
}`

func TestGeminiWebBuildPromptDescribesToolsAndHistory(t *testing.T) {
	plan, err := geminiWebBuildPrompt([]byte(geminiWebToolsBody))
	require.NoError(t, err)
	require.Equal(t, []string{"get_weather", "read_file"}, []string{plan.Tools[0].Name, plan.Tools[1].Name})
	require.False(t, plan.RequireTool)
	for _, want := range []string{
		"<system>\nBe brief.\n</system>",
		"- name: get_weather\n  description: Weather for a city.\n  parameters: {\"properties\":{\"city\":{\"type\":\"string\"}},\"type\":\"object\"}",
		"  description: Read a file.\n    Line two.",
		"<user>\nWeather in Paris?\n</user>",
		"<assistant>\n<tool_call>\n{\"arguments\":{\"city\":\"Paris\"},\"name\":\"get_weather\"}\n</tool_call>\n</assistant>",
		"<user>\n<tool_result name=\"get_weather\">\n18C, rain\n</tool_result>\n</user>",
		"Continue the conversation as the assistant.",
	} {
		require.Contains(t, plan.Prompt, want)
	}
	require.NotContains(t, plan.Prompt, "thinking...", "historic thoughts are not replayed")
}

func TestGeminiWebBuildPromptAppliesToolConfig(t *testing.T) {
	withConfig := func(config string) geminiWebPromptPlan {
		var root map[string]any
		require.NoError(t, json.Unmarshal([]byte(geminiWebToolsBody), &root))
		var cfg any
		require.NoError(t, json.Unmarshal([]byte(config), &cfg))
		root["toolConfig"] = cfg
		body, _ := json.Marshal(root)
		plan, err := geminiWebBuildPrompt(body)
		require.NoError(t, err)
		return plan
	}

	none := withConfig(`{"functionCallingConfig": {"mode": "NONE"}}`)
	require.Empty(t, none.Tools)
	require.NotContains(t, none.Prompt, "<tools>")
	require.Contains(t, none.Prompt, "Tool calling is not available", "history is still rendered as a transcript")

	only := withConfig(`{"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["read_file"]}}`)
	require.Len(t, only.Tools, 1)
	require.Equal(t, "read_file", only.Tools[0].Name)
	require.True(t, only.RequireTool)
	require.Contains(t, only.Prompt, "This reply must call at least one tool.")
	require.NotContains(t, only.Prompt, "- name: get_weather")
}

func TestGeminiWebBuildPromptKeepsPlainConversationsUnchanged(t *testing.T) {
	plan, err := geminiWebBuildPrompt([]byte(`{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"hello"}]},{"role":"user","parts":[{"text":"bye"}]}]}`))
	require.NoError(t, err)
	require.Equal(t, "sys\nuser: hi\nmodel: hello\nuser: bye", plan.Prompt)
	require.False(t, plan.toolsEnabled())

	_, err = geminiWebBuildPrompt([]byte(`{"contents":[{"role":"user","parts":[{"text":"look"},{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}]}`))
	require.ErrorContains(t, err, "text input only")
}

func TestGeminiWebExtractToolCalls(t *testing.T) {
	plan := geminiWebPromptPlan{Tools: []geminiWebTool{{Name: "get_weather"}, {Name: "read_file"}}}
	text := "Checking both.\n<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}\n</tool_call>\n" +
		"<tool_call>\n```json\n{\"name\": \"get_weather\", \"args\": \"{\\\"city\\\": \\\"Tokyo\\\"}\"}\n```\n</tool_call>\n" +
		"<tool_call>{\"name\": \"rm_rf\", \"arguments\": {}}</tool_call>\n" +
		"<tool_call>\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a_b.go\"}}"
	clean, calls := geminiWebExtractToolCalls(text, plan)
	require.Equal(t, []geminiWebToolCall{
		{Name: "get_weather", Args: map[string]any{"city": "Paris"}},
		{Name: "get_weather", Args: map[string]any{"city": "Tokyo"}},
		{Name: "read_file", Args: map[string]any{"path": "a_b.go"}},
	}, calls)
	require.Equal(t, "Checking both.\n\n\n<tool_call>{\"name\": \"rm_rf\", \"arguments\": {}}</tool_call>", clean, "unknown tools stay visible as text")

	clean, calls = geminiWebExtractToolCalls(text, geminiWebPromptPlan{})
	require.Equal(t, text, clean, "no tools offered: nothing is parsed")
	require.Empty(t, calls)
}

func TestGeminiWebStreamableTextHoldsBackToolCalls(t *testing.T) {
	require.Equal(t, "Sure <b>", geminiWebStreamableText("Sure <b>", true))
	require.Equal(t, "Sure", geminiWebStreamableText("Sure <tool_", true))
	require.Equal(t, "Sure", geminiWebStreamableText("Sure\n<tool_call>\n{\"name\"", true))
	require.Equal(t, "a\nb", geminiWebStreamableText("a\nb\n", true))
	require.Equal(t, "Sure <tool_", geminiWebStreamableText("Sure <tool_", false))
}

// geminiWebFramesStub serves a Gemini Web session whose StreamGenerate body
// carries one cumulative frame per snapshot.
func geminiWebFramesStub(t *testing.T, snapshots ...string) (*geminiWebUsageHTTPStub, *string) {
	t.Helper()
	var prompt string
	return &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
		body := `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`
		if req.Method == http.MethodPost {
			raw, _ := io.ReadAll(req.Body)
			form, err := url.ParseQuery(string(raw))
			require.NoError(t, err)
			var outer []any
			require.NoError(t, json.Unmarshal([]byte(form.Get("f.req")), &outer))
			var inner []any
			require.NoError(t, json.Unmarshal([]byte(outer[1].(string)), &inner))
			prompt = inner[0].([]any)[0].(string)
			body = ""
			for _, text := range snapshots {
				payload := make([]any, 43)
				payload[4] = []any{[]any{"candidate", []any{text}}}
				payload[39] = "cf41b0e0dd7d53e5"
				b, err := json.Marshal(payload)
				require.NoError(t, err)
				outer, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(b)}})
				require.NoError(t, err)
				body += string(outer) + "\n"
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}}, &prompt
}

var geminiWebToolAnswer = []string{
	"Let me check",
	"Let me check both cities.\n<tool_ca",
	"Let me check both cities.\n<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}\n</tool_call>",
	"Let me check both cities.\n<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}\n</tool_call>\n<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Tokyo\"}}\n</tool_call>",
}

func geminiWebTestAccount() *Account {
	return &Account{Platform: PlatformGemini, Type: AccountTypeGeminiWeb,
		Credentials: map[string]any{"gemini_web_cookie": "__Secure-1PSID=psid"}}
}

func TestGeminiWebClaudeToolCalls(t *testing.T) {
	const tools = `"tools":[{"name":"get_weather","description":"Weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			upstream, prompt := geminiWebFramesStub(t, geminiWebToolAnswer...)
			svc := &GeminiMessagesCompatService{httpUpstream: upstream}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			body := fmt.Sprintf(`{"model":"gemini-3.5-flash-lite","stream":%t,"max_tokens":64,%s,"messages":[
				{"role":"user","content":"Weather in Paris and Tokyo?"},
				{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Rome"}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"20C"}]}]}`, stream, tools)
			result, err := svc.Forward(context.Background(), c, geminiWebTestAccount(), []byte(body))
			require.NoError(t, err)
			require.Equal(t, "gemini-3.5-flash-lite", result.UpstreamResponseModel)
			require.Contains(t, *prompt, "<tool_result name=\"get_weather\">\n20C\n</tool_result>", "tool results reach the browser prompt")
			require.Contains(t, *prompt, "{\"arguments\":{\"city\":\"Rome\"},\"name\":\"get_weather\"}")
			out := recorder.Body.String()
			require.NotContains(t, out, "tool_call>", "tool markup never reaches the client")

			var blocks []map[string]any
			stopReason := ""
			if !stream {
				var resp map[string]any
				require.NoError(t, json.Unmarshal([]byte(out), &resp))
				for _, b := range resp["content"].([]any) {
					blocks = append(blocks, b.(map[string]any))
				}
				stopReason, _ = resp["stop_reason"].(string)
			} else {
				require.NotNil(t, result.FirstTokenMs)
				blocks, stopReason = collectClaudeSSEBlocks(t, out)
				require.Greater(t, strings.Count(out, `"type":"text_delta"`), 1, "text is streamed incrementally")
			}
			require.Equal(t, "tool_use", stopReason)
			require.Len(t, blocks, 3)
			require.Equal(t, "Let me check both cities.", blocks[0]["text"])
			for i, city := range []string{"Paris", "Tokyo"} {
				block := blocks[i+1]
				require.Equal(t, "tool_use", block["type"])
				require.Equal(t, "get_weather", block["name"])
				require.Equal(t, map[string]any{"city": city}, block["input"])
			}
			require.NotEqual(t, blocks[1]["id"], blocks[2]["id"])
		})
	}
}

// collectClaudeSSEBlocks rebuilds content blocks from an Anthropic SSE body,
// failing if a tool block's concatenated partial_json is not valid JSON.
func collectClaudeSSEBlocks(t *testing.T, body string) ([]map[string]any, string) {
	t.Helper()
	var blocks []map[string]any
	var partial []string
	stop := ""
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var evt map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt))
		switch evt["type"] {
		case "content_block_start":
			blocks = append(blocks, evt["content_block"].(map[string]any))
			partial = append(partial, "")
		case "content_block_delta":
			i := int(evt["index"].(float64))
			delta := evt["delta"].(map[string]any)
			if text, ok := delta["text"].(string); ok {
				blocks[i]["text"] = fmt.Sprint(blocks[i]["text"]) + text
			}
			if js, ok := delta["partial_json"].(string); ok {
				partial[i] += js
			}
		case "content_block_stop":
			i := int(evt["index"].(float64))
			if blocks[i]["type"] == "tool_use" {
				var input map[string]any
				require.NoError(t, json.Unmarshal([]byte(partial[i]), &input), "tool %d partial_json %q", i, partial[i])
				blocks[i]["input"] = input
			}
		case "message_delta":
			stop, _ = evt["delta"].(map[string]any)["stop_reason"].(string)
		}
	}
	return blocks, stop
}

func TestGeminiWebChatCompletionsToolCalls(t *testing.T) {
	const tools = `"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			upstream, _ := geminiWebFramesStub(t, geminiWebToolAnswer...)
			svc := &GeminiMessagesCompatService{httpUpstream: upstream}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			body := fmt.Sprintf(`{"model":"gemini-3.5-flash-lite","stream":%t,%s,"messages":[{"role":"user","content":"Weather in Paris and Tokyo?"}]}`, stream, tools)
			_, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiWebTestAccount(), []byte(body))
			require.NoError(t, err)
			out := recorder.Body.String()
			require.NotContains(t, out, "tool_call>")

			type call struct{ id, name, args string }
			var calls []call
			text, finish := "", ""
			if !stream {
				var resp struct {
					Choices []struct {
						FinishReason string `json:"finish_reason"`
						Message      struct {
							Content   string `json:"content"`
							ToolCalls []struct {
								ID       string `json:"id"`
								Function struct{ Name, Arguments string }
							} `json:"tool_calls"`
						}
					}
				}
				require.NoError(t, json.Unmarshal([]byte(out), &resp))
				msg := resp.Choices[0].Message
				text, finish = msg.Content, resp.Choices[0].FinishReason
				for _, tc := range msg.ToolCalls {
					calls = append(calls, call{tc.ID, tc.Function.Name, tc.Function.Arguments})
				}
			} else {
				require.True(t, strings.HasSuffix(out, "data: [DONE]\n\n"))
				byIndex := map[int]*call{}
				for _, line := range strings.Split(out, "\n") {
					if !strings.HasPrefix(line, "data: {") {
						continue
					}
					var chunk struct {
						Choices []struct {
							FinishReason *string `json:"finish_reason"`
							Delta        struct {
								Content   string `json:"content"`
								ToolCalls []struct {
									Index    int    `json:"index"`
									ID       string `json:"id"`
									Function struct{ Name, Arguments string }
								} `json:"tool_calls"`
							}
						}
					}
					require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk))
					for _, choice := range chunk.Choices {
						text += choice.Delta.Content
						if choice.FinishReason != nil {
							finish = *choice.FinishReason
						}
						for _, tc := range choice.Delta.ToolCalls {
							if byIndex[tc.Index] == nil {
								byIndex[tc.Index] = &call{}
							}
							c := byIndex[tc.Index]
							c.id += tc.ID
							c.name += tc.Function.Name
							c.args += tc.Function.Arguments
						}
					}
				}
				for i := 0; i < len(byIndex); i++ {
					calls = append(calls, *byIndex[i])
				}
			}
			require.Equal(t, "tool_calls", finish)
			require.Equal(t, "Let me check both cities.", text)
			require.Len(t, calls, 2)
			for i, city := range []string{"Paris", "Tokyo"} {
				require.Equal(t, "get_weather", calls[i].name)
				require.NotEmpty(t, calls[i].id)
				require.JSONEq(t, fmt.Sprintf(`{"city":%q}`, city), calls[i].args)
			}
		})
	}
}

func TestGeminiWebNativeToolCalls(t *testing.T) {
	const tools = `"tools":[{"functionDeclarations":[{"name":"get_weather","parameters":{"type":"object"}}]}]`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			upstream, _ := geminiWebFramesStub(t, geminiWebToolAnswer...)
			svc := &GeminiMessagesCompatService{httpUpstream: upstream}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			action := "generateContent"
			if stream {
				action = "streamGenerateContent"
			}
			_, err := svc.ForwardNative(context.Background(), c, geminiWebTestAccount(), "gemini-3.5-flash-lite", action, stream,
				[]byte(`{`+tools+`,"contents":[{"role":"user","parts":[{"text":"Weather?"}]}]}`))
			require.NoError(t, err)
			out := recorder.Body.String()
			require.NotContains(t, out, "tool_call>")
			require.Equal(t, 2, strings.Count(out, `"functionCall":`))
			require.Contains(t, out, `"functionCall":{"args":{"city":"Paris"},"name":"get_weather"}`)
			require.Contains(t, out, `"functionCall":{"args":{"city":"Tokyo"},"name":"get_weather"}`)
			if stream {
				require.Contains(t, out, `"text":"Let me check"`)
				require.Contains(t, out, `"text":" both cities."`)
			}
		})
	}
}

func TestGeminiWebCompatStreamFailsOverBeforeOutput(t *testing.T) {
	upstream := &geminiWebUsageHTTPStub{do: func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`))}, nil
	}}
	svc := &GeminiMessagesCompatService{httpUpstream: upstream}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	_, err := svc.Forward(context.Background(), c, geminiWebTestAccount(), []byte(`{"model":"gemini-3.5-flash-lite","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	var failover *UpstreamFailoverError
	require.True(t, errors.As(err, &failover), "err = %v", err)
	require.Equal(t, http.StatusTooManyRequests, failover.StatusCode)
	require.False(t, c.Writer.Written())
}

func TestGeminiWebPipeStreamKeepAliveBecomesClientPing(t *testing.T) {
	previous := geminiWebKeepAliveInterval
	geminiWebKeepAliveInterval = 20 * time.Millisecond
	defer func() { geminiWebKeepAliveInterval = previous }()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	svc := &GeminiMessagesCompatService{}
	plan := geminiWebPromptPlan{Tools: []geminiWebTool{{Name: "write_file"}}}
	pipe := newGeminiWebPipeStream(plan, false, func() {}, func(resp *http.Response) (*geminiStreamResult, error) {
		return svc.handleStreamingResponse(c, resp, time.Now(), "gemini-3.5-flash-lite")
	})
	require.NoError(t, pipe.OnResult(geminiweb.Result{Text: "Writing.\n<tool_call>\n{\"name\": \"write_file\""}))
	time.Sleep(120 * time.Millisecond)
	_, err := pipe.Finish(geminiweb.Result{Text: "Writing.\n<tool_call>\n{\"name\": \"write_file\", \"arguments\": {\"path\": \"a\"}}\n</tool_call>"}, ClaudeUsage{InputTokens: 1, OutputTokens: 2})
	require.NoError(t, err)
	out := recorder.Body.String()
	require.Contains(t, out, "event: ping", "a held-back tool call keeps the client stream alive")
	require.NotContains(t, out, geminiWebKeepAliveLine)
	blocks, stop := collectClaudeSSEBlocks(t, out)
	require.Equal(t, "tool_use", stop)
	require.Equal(t, map[string]any{"path": "a"}, blocks[1]["input"])
}

func TestGeminiWebClaudeToolChoice(t *testing.T) {
	gemini := []byte(`{"contents":[]}`)
	for choice, want := range map[string]string{
		`{"type":"none"}`:                    `{"functionCallingConfig":{"mode":"NONE"}}`,
		`{"type":"any"}`:                     `{"functionCallingConfig":{"mode":"ANY"}}`,
		`{"type":"tool","name":"read_file"}`: `{"functionCallingConfig":{"allowedFunctionNames":["read_file"],"mode":"ANY"}}`,
	} {
		var out map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(geminiWebClaudeToolChoice(gemini, []byte(`{"tool_choice":`+choice+`}`)), &out))
		require.JSONEq(t, want, string(out["toolConfig"]))
	}
	require.Equal(t, gemini, geminiWebClaudeToolChoice(gemini, []byte(`{"tool_choice":{"type":"auto"}}`)))
}

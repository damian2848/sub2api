//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrismResponsesCustomExecRoundTrip(t *testing.T) {
	program := "text(await tools.exec_command({cmd: \"pwd\"}));\n"
	description := "Run JavaScript in the executor.\n\n" + strings.Repeat("Runtime contract. ", 100) +
		"\nconst result = await tools.exec_command({cmd: \"pwd\"});\ntext(result);"
	for _, additional := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("additional_%t_stream_%t", additional, stream), func(t *testing.T) {
				h := newPrismGatewayHarness(t, nil, nil)
				tool := map[string]any{"type": "custom", "name": "exec", "description": description,
					"format": map[string]any{"type": "text"}}
				call := map[string]any{"type": "custom_tool_call", "id": "ctc_previous", "call_id": "call_previous",
					"name": "exec", "input": program}
				input := []any{map[string]any{"role": "user", "content": "Print the current directory."}}
				body := map[string]any{"model": "gpt-6.1-sol", "stream": stream}
				upstreamName := "exec"
				if additional {
					body["tools"] = []any{}
					input = append(input, map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{
						map[string]any{"type": "namespace", "name": "functions", "tools": []any{tool}},
					}})
					call["namespace"] = "functions"
				} else {
					body["tools"] = []any{tool}
				}
				body["input"] = append(input, call, map[string]any{"type": "custom_tool_call_output", "call_id": "call_previous", "output": "/repo"})
				payload, err := json.Marshal(body)
				require.NoError(t, err)
				arguments, err := json.Marshal(map[string]string{"input": program})
				require.NoError(t, err)
				item := map[string]any{"type": "function_call", "id": "fc_exec", "call_id": "call_exec",
					"name": upstreamName, "arguments": string(arguments), "status": "completed"}
				response := map[string]any{"id": "resp_exec", "object": "response", "created_at": 1,
					"model": "gpt-6.1-sol", "status": "completed", "output": []any{item},
					"usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}
				wire, err := json.Marshal(response)
				require.NoError(t, err)
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					wire = nil
					for sequence, event := range []map[string]any{
						{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{
							"type": "function_call", "id": "fc_exec", "call_id": "call_exec", "name": upstreamName, "arguments": "", "status": "in_progress"}},
						{"type": "response.function_call_arguments.done", "item_id": "fc_exec", "output_index": 0,
							"call_id": "call_exec", "name": upstreamName, "arguments": string(arguments)},
						{"type": "response.output_item.done", "output_index": 0, "item": item},
						{"type": "response.completed", "response": response},
					} {
						event["sequence_number"] = sequence
						encoded, err := json.Marshal(event)
						require.NoError(t, err)
						wire = append(wire, []byte("data: "+string(encoded)+"\n\n")...)
					}
				}
				h.upstreamAnswer = &http.Response{StatusCode: http.StatusOK,
					Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(string(wire)))}
				recorder, _, err := h.forward("responses", "/v1/responses", payload, nil)
				require.NoError(t, err)
				upstream := h.upstreamBody(t)
				require.Equal(t, "function", gjson.GetBytes(upstream, "tools.0.type").String())
				require.Equal(t, upstreamName, gjson.GetBytes(upstream, "tools.0.name").String())
				require.Equal(t, "string", gjson.GetBytes(upstream, "tools.0.parameters.properties.input.type").String())
				require.Equal(t, description, gjson.GetBytes(upstream, "tools.0.description").String())
				history := gjson.GetBytes(upstream, `input.#(call_id=="call_previous")`)
				require.Equal(t, "function_call", history.Get("type").String())
				require.Equal(t, program, gjson.Parse(history.Get("arguments").String()).Get("input").String())
				if stream {
					require.Contains(t, recorder.Body.String(), `"type":"response.custom_tool_call_input.delta"`)
					require.Contains(t, recorder.Body.String(), `"type":"response.custom_tool_call_input.done"`)
					sawDone, sawItem := false, false
					for _, line := range strings.Split(recorder.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						event := gjson.Parse(strings.TrimPrefix(line, "data: "))
						if event.Get("type").String() == "response.custom_tool_call_input.done" {
							sawDone = true
							require.Equal(t, program, event.Get("input").String())
							require.Equal(t, "exec", event.Get("name").String())
						}
						if event.Get("type").String() == "response.output_item.done" {
							sawItem = true
							require.Equal(t, "custom_tool_call", event.Get("item.type").String())
							require.Equal(t, "exec", event.Get("item.name").String())
							require.Equal(t, program, event.Get("item.input").String())
							require.False(t, event.Get("item.namespace").Exists(), "default functions tools use the public unqualified identity")
						}
					}
					require.True(t, sawDone)
					require.True(t, sawItem)
				} else {
					output := gjson.Get(recorder.Body.String(), "output.0")
					require.Equal(t, "custom_tool_call", output.Get("type").String())
					require.Equal(t, "exec", output.Get("name").String())
					require.Equal(t, program, output.Get("input").String())
					require.Equal(t, "ctc_exec", output.Get("id").String())
					require.False(t, output.Get("namespace").Exists(), "default functions tools use the public unqualified identity")
				}
			})
		}
	}
}

func TestAdaptPrismResponsesClientToolsLeavesFunctionsUnchanged(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","input":"hi","tools":[{"type":"function","name":"get_weather","parameters":{}}]}`)
	adapted, mapping, err := adaptPrismResponsesClientTools(body)
	require.NoError(t, err)
	require.Equal(t, body, adapted)
	require.Empty(t, mapping)
}

func TestPrismWSHTTPBridgeCustomExecRoundTripAndContinuation(t *testing.T) {
	program := "text(await tools.exec_command({cmd: \"pwd\"}));\n"
	description := "Run JavaScript in the executor.\n\nconst result = await tools.exec_command({cmd: \"pwd\"});\ntext(result);"
	for _, additional := range []bool{false, true} {
		t.Run(fmt.Sprintf("additional_%t", additional), func(t *testing.T) {
			h := newPrismGatewayHarness(t, nil, nil)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			tool := map[string]any{"type": "custom", "name": "exec", "description": description,
				"format": map[string]any{"type": "text"}}
			first := map[string]any{"type": "response.create", "model": "gpt-6.1-sol",
				"input": []any{map[string]any{"role": "user", "content": "Print the current directory."}}}
			if additional {
				first["tools"] = []any{}
				first["input"] = append(first["input"].([]any), map[string]any{"type": "additional_tools", "role": "developer",
					"tools": []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{tool}}}})
			} else {
				first["tools"] = []any{tool}
			}

			var previousCall json.RawMessage
			for turn := 1; turn <= 2; turn++ {
				request := first
				if turn == 2 {
					// Ingress has already expanded Prism's replay history and removed
					// previous_response_id before dispatching a bridge continuation.
					request = map[string]any{"type": "response.create", "model": "gpt-6.1-sol",
						"input": []any{previousCall, map[string]any{"type": "custom_tool_call_output", "call_id": "call_exec_1", "output": "/repo"}}}
				}
				payload, err := json.Marshal(request)
				require.NoError(t, err)
				arguments, err := json.Marshal(map[string]string{"input": program})
				require.NoError(t, err)
				itemID, callID := fmt.Sprintf("fc_exec_%d", turn), fmt.Sprintf("call_exec_%d", turn)
				item := map[string]any{"type": "function_call", "id": itemID, "call_id": callID,
					"name": "exec", "arguments": string(arguments), "status": "completed"}
				response := map[string]any{"id": fmt.Sprintf("resp_exec_%d", turn), "model": "gpt-6.1-sol", "status": "completed",
					"output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}
				var wire strings.Builder
				for sequence, event := range []map[string]any{
					{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{
						"type": "function_call", "id": itemID, "call_id": callID, "name": "exec", "arguments": "", "status": "in_progress"}},
					{"type": "response.function_call_arguments.done", "item_id": itemID, "output_index": 0,
						"call_id": callID, "name": "exec", "arguments": string(arguments)},
					{"type": "response.output_item.done", "output_index": 0, "item": item},
					{"type": "response.completed", "response": response},
				} {
					event["sequence_number"] = sequence
					encoded, err := json.Marshal(event)
					require.NoError(t, err)
					wire.WriteString("data: " + string(encoded) + "\n\n")
				}
				h.upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(wire.String()))}
				var events [][]byte
				result, err := h.svc.proxyOpenAIWSHTTPBridgeTurn(h.ctx, c, h.account, "test-token", payload, len(payload),
					"gpt-6.1-sol", "", "", "", "", turn, func(message []byte) error {
						events = append(events, append([]byte(nil), message...))
						return nil
					})
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, "function", gjson.GetBytes(h.upstream.lastBody, "tools.0.type").String())
				require.Equal(t, "exec", gjson.GetBytes(h.upstream.lastBody, "tools.0.name").String())
				require.Equal(t, description, gjson.GetBytes(h.upstream.lastBody, "tools.0.description").String())
				require.Equal(t, "string", gjson.GetBytes(h.upstream.lastBody, "tools.0.parameters.properties.input.type").String())
				if turn == 2 {
					require.False(t, gjson.GetBytes(payload, "tools").Exists(), "the continuation inherits the first turn's declarations")
					require.Equal(t, "function_call", gjson.GetBytes(h.upstream.lastBody, "input.0.type").String())
					require.Equal(t, program, gjson.Parse(gjson.GetBytes(h.upstream.lastBody, "input.0.arguments").String()).Get("input").String())
					require.Equal(t, "function_call_output", gjson.GetBytes(h.upstream.lastBody, "input.1.type").String())
				}
				state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
				require.True(t, ok)
				require.True(t, state.ClientMapping.CustomTools["exec"])
				sawInputDone, sawItemDone, sawCompleted := false, false, false
				for _, event := range events {
					parsed := gjson.ParseBytes(event)
					switch parsed.Get("type").String() {
					case "response.custom_tool_call_input.done":
						sawInputDone = true
						require.Equal(t, program, parsed.Get("input").String())
					case "response.output_item.done", "response.completed":
						path := "item"
						if parsed.Get("type").String() == "response.completed" {
							sawCompleted = true
							path = "response.output.0"
						} else {
							sawItemDone = true
						}
						call := parsed.Get(path)
						require.Equal(t, "custom_tool_call", call.Get("type").String())
						require.Equal(t, "exec", call.Get("name").String())
						require.Equal(t, fmt.Sprintf("ctc_exec_%d", turn), call.Get("id").String())
						require.Equal(t, program, call.Get("input").String())
						require.False(t, call.Get("namespace").Exists())
						previousCall = json.RawMessage(call.Raw)
					}
				}
				require.True(t, sawInputDone)
				require.True(t, sawItemDone)
				require.True(t, sawCompleted)
				require.True(t, result.wsReplayInputExists)
				require.Len(t, result.wsReplayInput, 1)
				require.Equal(t, "custom_tool_call", gjson.GetBytes(result.wsReplayInput[0], "type").String())
			}
		})
	}
}

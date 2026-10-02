package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnthropicDocumentsReachResponsesIncludingToolResults(t *testing.T) {
	raw := `{"model":"gpt-6.1-sol","max_tokens":64,"messages":[{"role":"user","content":[
		{"type":"document","title":"notes.pdf","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}},
		{"type":"document","source":{"type":"text","media_type":"text/plain","data":"hello"}},
		{"type":"image","source":{"type":"url","url":"https://example.test/image.png"}},
		{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"downloaded"},
			{"type":"document","source":{"type":"url","url":"https://example.test/report.pdf"}}]}]}]}`
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	response, err := AnthropicToResponses(&req)
	if err != nil {
		t.Fatal(err)
	}
	var input []ResponsesInputItem
	if err := json.Unmarshal(response.Input, &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 2 || input[0].Output != "downloaded" {
		t.Fatalf("tool output was lost: %#v", input)
	}
	var parts []ResponsesContentPart
	if err := json.Unmarshal(input[1].Content, &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 4 || parts[0].Type != "input_file" || parts[0].Filename != "notes.pdf" ||
		parts[0].FileData != "data:application/pdf;base64,AAAA" || parts[1].FileData != "data:text/plain;base64,aGVsbG8=" ||
		parts[2].ImageURL != "https://example.test/image.png" || parts[3].FileURL != "https://example.test/report.pdf" {
		t.Fatalf("attachment conversion changed: %#v", parts)
	}
	encoded, err := json.Marshal(response)
	if err != nil || !strings.Contains(string(encoded), `"file_url":"https://example.test/report.pdf"`) {
		t.Fatalf("URL was lost on the wire: %s, %v", encoded, err)
	}
}

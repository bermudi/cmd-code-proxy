package proxy

import (
	"reflect"
	"testing"

	"github.com/bermudi/cmd-code-proxy/internal/api"
)

func TestParseContent_ReasoningParts(t *testing.T) {
	// When OpenAI message contains thinking/reasoning content parts,
	// they should map to CC type "reasoning" (not collapsed to "text").
	content := []any{
		map[string]any{"type": "text", "text": "Hello"},
		map[string]any{"type": "thinking", "thinking": "Let me think..."},
		map[string]any{"type": "reasoning", "reasoning": "Step 1: analyze"},
	}
	parts := parseContent(content, nil)

	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(parts))
	}

	if parts[0].Type != "text" {
		t.Errorf("part[0].Type = %q, want text", parts[0].Type)
	}
	if parts[1].Type != "reasoning" {
		t.Errorf("part[1].Type = %q, want reasoning", parts[1].Type)
	}
	if parts[1].Text == nil || *parts[1].Text != "Let me think..." {
		t.Errorf("part[1].Text = %v, want 'Let me think...'", parts[1].Text)
	}
	if parts[2].Type != "reasoning" {
		t.Errorf("part[2].Type = %q, want reasoning", parts[2].Type)
	}
}

func TestConvertMessages_AssistantWithToolCalls(t *testing.T) {
	msgs := []api.OpenAIMessage{
		{
			Role: "assistant",
			Content: []any{
				map[string]any{"type": "text", "text": "I'll help"},
			},
			ToolCalls: []api.ToolCall{
				{ID: "tc1", Type: "function", Function: api.FunctionCall{Name: "read_file", Arguments: `{"path": "/tmp/test"}`}},
			},
		},
	}

	ccMsgs := ConvertMessages(msgs)
	if len(ccMsgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(ccMsgs))
	}
	if len(ccMsgs[0].Content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(ccMsgs[0].Content))
	}
	if ccMsgs[0].Content[0].Type != "text" {
		t.Errorf("content[0].Type = %q, want text", ccMsgs[0].Content[0].Type)
	}
	if ccMsgs[0].Content[1].Type != "tool-call" {
		t.Errorf("content[1].Type = %q, want tool-call", ccMsgs[0].Content[1].Type)
	}
	if ccMsgs[0].Content[1].ToolCallID == nil || *ccMsgs[0].Content[1].ToolCallID != "tc1" {
		t.Errorf("content[1].ToolCallID = %v, want tc1", ccMsgs[0].Content[1].ToolCallID)
	}
}

func TestDropSystemMessages(t *testing.T) {
	msgs := []api.OpenAIMessage{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hi"},
		{Role: "developer", Content: "Be concise."},
		{Role: "assistant", Content: "Hello"},
		{Role: "tool", Content: "result"},
		{Role: "user", Content: "Bye"},
	}
	got := DropSystemMessages(msgs)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (system+developer dropped)", len(got))
	}
	wantRoles := []string{"user", "assistant", "tool", "user"}
	for i, m := range got {
		if m.Role != wantRoles[i] {
			t.Errorf("got[%d].Role = %q, want %q", i, m.Role, wantRoles[i])
		}
	}
}

func TestDropSystemMessages_AllSystem(t *testing.T) {
	msgs := []api.OpenAIMessage{
		{Role: "system", Content: "a"},
		{Role: "developer", Content: "b"},
	}
	got := DropSystemMessages(msgs)
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestNormalizeRole(t *testing.T) {
	cases := []struct{ in, want string }{
		{"user", "user"},
		{"assistant", "assistant"},
		{"tool", "tool"},
		// Anything else is a programmer error — DropSystemMessages should
		// have stripped system/developer before this is called. We return
		// "" so the caller (ConvertMessages) drops the message rather than
		// forwarding a bogus role upstream.
		{"system", ""},
		{"developer", ""},
		{"function", ""},
		{"", ""},
		{"unknown", ""},
	}
	for _, c := range cases {
		got := normalizeRole(c.in)
		if got != c.want {
			t.Errorf("normalizeRole(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestConvertMessages_DropsInvalidRoles is a belt-and-suspenders check:
// if a non-CC-valid role ever reaches ConvertMessages, the message is
// dropped (not rewritten to "user" and forwarded as the AGENTS.md-leak
// bug).
func TestConvertMessages_DropsInvalidRoles(t *testing.T) {
	msgs := []api.OpenAIMessage{
		{Role: "system", Content: "leak attempt"},
		{Role: "user", Content: "hello"},
		{Role: "function", Content: "another leak"},
		{Role: "assistant", Content: "hi back"},
	}
	ccMsgs := ConvertMessages(msgs)
	if len(ccMsgs) != 2 {
		t.Fatalf("len = %d, want 2 (system + function dropped, user + assistant kept)", len(ccMsgs))
	}
	if ccMsgs[0].Role != "user" {
		t.Errorf("ccMsgs[0].Role = %q, want user", ccMsgs[0].Role)
	}
	if ccMsgs[1].Role != "assistant" {
		t.Errorf("ccMsgs[1].Role = %q, want assistant", ccMsgs[1].Role)
	}
}

func TestParseToolInputJSON(t *testing.T) {
	// nil → empty map
	if m, ok := parseToolInputJSON(nil).(map[string]any); !ok || len(m) != 0 {
		t.Errorf("parseToolInputJSON(nil) = %v, want empty map", parseToolInputJSON(nil))
	}
	// map → passed through
	input := map[string]any{"path": "/tmp"}
	if got := parseToolInputJSON(input); !reflect.DeepEqual(got, input) {
		t.Errorf("parseToolInputJSON(map) returned different map")
	}
	// JSON string → parsed
	if m, ok := parseToolInputJSON(`{"path":"/tmp"}`).(map[string]any); !ok {
		t.Errorf("parseToolInputJSON(string) didn't parse JSON")
	} else if m["path"] != "/tmp" {
		t.Errorf("parseToolInputJSON(string) path = %v, want /tmp", m["path"])
	}
	// invalid string → wrapped
	if m, ok := parseToolInputJSON("not-json").(map[string]any); !ok || m["arguments"] != "not-json" {
		t.Errorf("parseToolInputJSON(invalid) = %v, want wrapped arguments", parseToolInputJSON("not-json"))
	}
}

func TestParseContent_ToolCallOpenAIFormat(t *testing.T) {
	// OpenAI format: type "tool_call" with nested function object
	content := []any{
		map[string]any{
			"type": "tool_call",
			"id":   "call_abc",
			"function": map[string]any{
				"name":      "read_file",
				"arguments": `{"path": "/tmp"}`,
			},
		},
	}
	parts := parseContent(content, nil)
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0].Type != "tool-call" {
		t.Errorf("type = %q, want tool-call", parts[0].Type)
	}
	if parts[0].ToolCallID == nil || *parts[0].ToolCallID != "call_abc" {
		t.Errorf("toolCallId = %v, want call_abc", parts[0].ToolCallID)
	}
	if parts[0].ToolName == nil || *parts[0].ToolName != "read_file" {
		t.Errorf("toolName = %v, want read_file", parts[0].ToolName)
	}
	// arguments should be parsed JSON
	if m, ok := parts[0].Input.(map[string]any); !ok || m["path"] != "/tmp" {
		t.Errorf("input = %v, want parsed map", parts[0].Input)
	}
}

func TestParseContent_SkipsInvalidToolCall(t *testing.T) {
	// Missing id and name → should be skipped, not emitted with nil fields
	content := []any{
		map[string]any{"type": "tool-call"},
	}
	parts := parseContent(content, nil)
	if len(parts) != 0 {
		t.Fatalf("expected 0 parts (invalid tool-call skipped), got %d", len(parts))
	}
}

func TestConvertMessages_SkipsToolWithEmptyName(t *testing.T) {
	msgs := []api.OpenAIMessage{
		{
			Role: "assistant",
			ToolCalls: []api.ToolCall{
				{ID: "tc1", Type: "function", Function: api.FunctionCall{Name: "", Arguments: "{}"}},
				{ID: "tc2", Type: "function", Function: api.FunctionCall{Name: "valid_tool", Arguments: "{}"}},
			},
		},
	}
	ccMsgs := ConvertMessages(msgs)
	if len(ccMsgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(ccMsgs))
	}
	if len(ccMsgs[0].Content) != 1 {
		t.Fatalf("expected 1 content part (empty name skipped), got %d", len(ccMsgs[0].Content))
	}
	if ccMsgs[0].Content[0].ToolCallID == nil || *ccMsgs[0].Content[0].ToolCallID != "tc2" {
		t.Errorf("expected tc2, got %v", ccMsgs[0].Content[0].ToolCallID)
	}
}

func TestConvertMessages_ToolResult(t *testing.T) {
	msgs := []api.OpenAIMessage{
		{
			Role: "assistant",
			Content: []any{
				map[string]any{"type": "text", "text": "Done"},
				map[string]any{"type": "tool-call", "toolCallId": "tc1", "toolName": "read_file", "input": map[string]any{"path": "/tmp/a"}},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "tc1",
			Content:    "file contents here",
		},
	}

	ccMsgs := ConvertMessages(msgs)
	if len(ccMsgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(ccMsgs))
	}
	if ccMsgs[1].Role != "tool" {
		t.Errorf("msg[1].Role = %q, want tool", ccMsgs[1].Role)
	}
	if len(ccMsgs[1].Content) != 1 {
		t.Fatalf("expected 1 content part, got %d", len(ccMsgs[1].Content))
	}
	if ccMsgs[1].Content[0].Type != "tool-result" {
		t.Errorf("content[0].Type = %q, want tool-result", ccMsgs[1].Content[0].Type)
	}
	if ccMsgs[1].Content[0].Output == nil || ccMsgs[1].Content[0].Output.Value != "file contents here" {
		t.Errorf("content[0].Output.Value = %v, want 'file contents here'", ccMsgs[1].Content[0].Output)
	}
	if ccMsgs[1].Content[0].Output.Type != "text" {
		t.Errorf("content[0].Output.Type = %q, want text", ccMsgs[1].Content[0].Output.Type)
	}
}

func TestConvertMessages_ToolResultError(t *testing.T) {
	msgs := []api.OpenAIMessage{
		{
			Role:       "tool",
			ToolCallID: "tc1",
			Content:    "Error: file not found",
		},
	}

	ccMsgs := ConvertMessages(msgs)
	if ccMsgs[0].Content[0].Output.Type != "error-text" {
		t.Errorf("error output type = %q, want error-text", ccMsgs[0].Content[0].Output.Type)
	}
}

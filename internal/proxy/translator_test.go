package proxy

import (
	"io"
	"strings"
	"testing"
)

func TestEventTranslator_TextDelta(t *testing.T) {
	lines := `{"type":"text-delta","text":"hello"}`
	tr := NewEventTranslator(strings.NewReader(lines))

	if !tr.Next() {
		t.Fatal("expected one event")
	}
	e := tr.Event()
	if e.Type != EventTextDelta {
		t.Errorf("type = %v, want EventTextDelta", e.Type)
	}
	if e.Text != "hello" {
		t.Errorf("text = %q, want %q", e.Text, "hello")
	}
	if tr.Next() {
		t.Error("expected no more events")
	}
}

func TestEventTranslator_ReasoningDelta(t *testing.T) {
	lines := joinLines(
		`{"type":"reasoning-start"}`,
		`{"type":"reasoning-delta","text":"thinking..."}`,
		`{"type":"reasoning-end"}`,
	)
	tr := NewEventTranslator(strings.NewReader(lines))

	events := collectEvents(t, tr)
	wantTypes := []EventType{EventReasoningStart, EventReasoningDelta, EventReasoningEnd}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d", len(events), len(wantTypes))
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event[%d].Type = %v, want %v", i, events[i].Type, want)
		}
	}
	if events[1].Text != "thinking..." {
		t.Errorf("reasoning text = %q, want %q", events[1].Text, "thinking...")
	}
}

func TestEventTranslator_ToolUse(t *testing.T) {
	lines := joinLines(
		`{"type":"tool-use","toolCallId":"call_1","toolName":"get_weather"}`,
		`{"type":"tool-delta","text":"{\"city\":\"SF\"}"}`,
	)
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if events[0].Type != EventToolUse {
		t.Fatalf("event[0] type = %v, want EventToolUse", events[0].Type)
	}
	if events[0].ToolCallID != "call_1" {
		t.Errorf("toolCallID = %q, want %q", events[0].ToolCallID, "call_1")
	}
	if events[0].ToolName != "get_weather" {
		t.Errorf("toolName = %q, want %q", events[0].ToolName, "get_weather")
	}

	if events[1].Type != EventToolDelta {
		t.Fatalf("event[1] type = %v, want EventToolDelta", events[1].Type)
	}
	if events[1].Text != `{"city":"SF"}` {
		t.Errorf("tool-delta text = %q, want %q", events[1].Text, `{"city":"SF"}`)
	}
}

func TestEventTranslator_ToolInputStartDelta(t *testing.T) {
	lines := joinLines(
		`{"type":"tool-input-start","id":"in_1","toolName":"search"}`,
		`{"type":"tool-input-delta","id":"in_1","delta":"{\"q\":"}`,
		`{"type":"tool-input-delta","id":"in_1","delta":"\"test\"}"}`,
	)
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if events[0].Type != EventToolInputStart {
		t.Fatalf("event[0] type = %v, want EventToolInputStart", events[0].Type)
	}
	if events[0].ID != "in_1" || events[0].ToolName != "search" {
		t.Errorf("id=%q toolName=%q, want in_1/search", events[0].ID, events[0].ToolName)
	}

	if events[1].Type != EventToolInputDelta {
		t.Fatalf("event[1] type = %v, want EventToolInputDelta", events[1].Type)
	}
	if events[1].Delta != `{"q":` {
		t.Errorf("delta = %q, want %q", events[1].Delta, `{"q":`)
	}

	if events[2].Delta != `"test"}` {
		t.Errorf("delta = %q, want %q", events[2].Delta, `"test"}`)
	}
}

func TestEventTranslator_ToolCall(t *testing.T) {
	lines := `{"type":"tool-call","toolCallId":"tc_1","toolName":"read","input":{"path":"/tmp/f.go"}}`
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if events[0].Type != EventToolCall {
		t.Fatalf("type = %v, want EventToolCall", events[0].Type)
	}
	if events[0].ToolCallID != "tc_1" {
		t.Errorf("toolCallID = %q, want %q", events[0].ToolCallID, "tc_1")
	}
	if events[0].ToolName != "read" {
		t.Errorf("toolName = %q, want %q", events[0].ToolName, "read")
	}
	if events[0].Input["path"] != "/tmp/f.go" {
		t.Errorf("input = %v, want path=/tmp/f.go", events[0].Input)
	}
}

func TestEventTranslator_Finish(t *testing.T) {
	lines := `{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":100,"outputTokens":50,"cachedInputTokens":10,"cacheCreationInputTokens":5}}`
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if events[0].Type != EventFinish {
		t.Fatalf("type = %v, want EventFinish", events[0].Type)
	}
	if events[0].FinishReason != "stop" {
		t.Errorf("finishReason = %q, want %q", events[0].FinishReason, "stop")
	}
	u := events[0].Usage
	if u == nil {
		t.Fatal("usage is nil")
	}
	if u.InputTokens != 100 || u.OutputTokens != 50 {
		t.Errorf("tokens = %d/%d, want 100/50", u.InputTokens, u.OutputTokens)
	}
	if u.CacheReadInputTokens != 10 || u.CacheCreationInputTokens != 5 {
		t.Errorf("cache tokens = %d/%d, want 10/5", u.CacheReadInputTokens, u.CacheCreationInputTokens)
	}
}

func TestEventTranslator_FinishStep(t *testing.T) {
	// finish-step uses "usage" key instead of "totalUsage"
	lines := `{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":5320,"outputTokens":42,"cachedInputTokens":5234}}`
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if events[0].Type != EventFinish {
		t.Fatalf("type = %v, want EventFinish", events[0].Type)
	}
	if events[0].FinishReason != "stop" {
		t.Errorf("finishReason = %q, want %q", events[0].FinishReason, "stop")
	}
	u := events[0].Usage
	if u == nil {
		t.Fatal("usage is nil")
	}
	if u.InputTokens != 5320 || u.OutputTokens != 42 {
		t.Errorf("tokens = %d/%d, want 5320/42", u.InputTokens, u.OutputTokens)
	}
	if u.CacheReadInputTokens != 5234 {
		t.Errorf("cacheRead = %d, want 5234", u.CacheReadInputTokens)
	}
}

func TestEventTranslator_FinishStepThenFinish_Deduplicates(t *testing.T) {
	// Upstream sends both finish-step and finish with identical data.
	// The translator should deduplicate — only the first one is emitted.
	lines := "{\"type\":\"finish-step\",\"finishReason\":\"tool-calls\",\"rawFinishReason\":\"tool_use\",\"usage\":{\"inputTokens\":100,\"outputTokens\":50}}\n{\"type\":\"finish\",\"finishReason\":\"tool-calls\",\"rawFinishReason\":\"tool_use\",\"totalUsage\":{\"inputTokens\":100,\"outputTokens\":50}}"
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if len(events) != 1 {
		t.Fatalf("expected 1 event (deduplicated), got %d", len(events))
	}
	if events[0].Type != EventFinish {
		t.Fatalf("expected EventFinish, got %v", events[0].Type)
	}
	// The first event was finish-step with usage, so usage should be populated.
	if events[0].Usage == nil {
		t.Fatal("expected usage from finish-step to be preserved")
	}
	if events[0].Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", events[0].Usage.InputTokens)
	}
}

func TestEventTranslator_FinishStepBareThenFinishWithUsage(t *testing.T) {
	// Edge case: finish-step arrives without usage, finish brings it.
	// The translator should skip the bare finish-step and emit the finish.
	lines := "{\"type\":\"finish-step\",\"finishReason\":\"stop\"}\n{\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":100,\"outputTokens\":50,\"cachedInputTokens\":10}}"
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if len(events) != 1 {
		t.Fatalf("expected 1 event (deduplicated), got %d", len(events))
	}
	if events[0].Type != EventFinish {
		t.Fatalf("expected EventFinish, got %v", events[0].Type)
	}
	if events[0].Usage == nil {
		t.Fatal("expected usage from finish to be preserved after skipping bare finish-step")
	}
	if events[0].Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", events[0].Usage.InputTokens)
	}
	if events[0].Usage.CacheReadInputTokens != 10 {
		t.Errorf("CacheReadInputTokens = %d, want 10", events[0].Usage.CacheReadInputTokens)
	}
}

func TestEventTransformer_FinishNoUsage(t *testing.T) {
	lines := `{"type":"finish","finishReason":"length"}`
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if events[0].Usage != nil {
		t.Errorf("expected nil usage, got %v", events[0].Usage)
	}
}

func TestEventTranslator_Error(t *testing.T) {
	code := 429
	lines := joinLines(
		`{"type":"error","error":{"message":"rate limited","statusCode":429}}`,
	)
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if events[0].Type != EventError {
		t.Fatalf("type = %v, want EventError", events[0].Type)
	}
	if events[0].Error == nil {
		t.Fatal("error is nil")
	}
	if events[0].Error.Message != "rate limited" {
		t.Errorf("message = %q, want %q", events[0].Error.Message, "rate limited")
	}
	if events[0].Error.StatusCode == nil || *events[0].Error.StatusCode != code {
		t.Errorf("statusCode = %v, want %d", events[0].Error.StatusCode, code)
	}
}

func TestEventTranslator_ToolResult(t *testing.T) {
	lines := `{"type":"tool-result"}`
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if events[0].Type != EventToolResult {
		t.Fatalf("type = %v, want EventToolResult", events[0].Type)
	}
}

func TestEventTranslator_SkipsBlankLines(t *testing.T) {
	lines := "\n\n  \n{\"type\":\"text-delta\",\"text\":\"hi\"}\n\n"
	tr := NewEventTranslator(strings.NewReader(lines))

	if !tr.Next() {
		t.Fatal("expected one event")
	}
	if tr.Event().Text != "hi" {
		t.Errorf("text = %q, want %q", tr.Event().Text, "hi")
	}
	if tr.Next() {
		t.Error("expected no more events")
	}
}

func TestEventTranslator_SkipsInvalidJSON(t *testing.T) {
	lines := joinLines(
		"not json at all",
		`{"type":"text-delta","text":"ok"}`,
		`{bad`,
	)
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Text != "ok" {
		t.Errorf("text = %q, want %q", events[0].Text, "ok")
	}
}

func TestEventTranslator_SkipsUnknownType(t *testing.T) {
	lines := joinLines(
		`{"type":"text-delta","text":"before"}`,
		`{"type":"some-new-type","data":"whatever"}`,
		`{"type":"text-delta","text":"after"}`,
	)
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Text != "before" || events[1].Text != "after" {
		t.Errorf("texts = %q, %q, want before, after", events[0].Text, events[1].Text)
	}
}

func TestEventTranslator_EmptyStream(t *testing.T) {
	tr := NewEventTranslator(strings.NewReader(""))
	if tr.Next() {
		t.Error("expected no events from empty stream")
	}
	if tr.Err() != nil {
		t.Errorf("unexpected error: %v", tr.Err())
	}
}

func TestEventTranslator_ReadError(t *testing.T) {
	tr := NewEventTranslator(&errorReader{err: io.ErrUnexpectedEOF})
	if tr.Next() {
		t.Error("expected no events from error reader")
	}
	if tr.Err() != io.ErrUnexpectedEOF {
		t.Errorf("err = %v, want ErrUnexpectedEOF", tr.Err())
	}
}

func TestEventTranslator_RawLine(t *testing.T) {
	raw := `{"type":"text-delta","text":"x"}`
	tr := NewEventTranslator(strings.NewReader(raw))
	if !tr.Next() {
		t.Fatal("expected event")
	}
	if tr.Event().RawLine != raw {
		t.Errorf("rawLine = %q, want %q", tr.Event().RawLine, raw)
	}
}

func TestEventTranslator_FullConversation(t *testing.T) {
	lines := joinLines(
		`{"type":"reasoning-start"}`,
		`{"type":"reasoning-delta","text":"Let me check the files."}`,
		`{"type":"reasoning-end"}`,
		`{"type":"text-delta","text":"I found "}`,
		`{"type":"text-delta","text":"3 issues."}`,
		`{"type":"tool-input-start","id":"c1","toolName":"read_file"}`,
		`{"type":"tool-input-delta","id":"c1","delta":"{\"path\":\"a.go\"}"}`,
		`{"type":"tool-result"}`,
		`{"type":"text-delta","text":"Done."}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":50,"outputTokens":20}}`,
	)
	tr := NewEventTranslator(strings.NewReader(lines))
	events := collectEvents(t, tr)

	wantCount := 10
	if len(events) != wantCount {
		t.Fatalf("got %d events, want %d", len(events), wantCount)
	}

	last := events[len(events)-1]
	if last.Type != EventFinish {
		t.Errorf("last event type = %v, want EventFinish", last.Type)
	}
	if last.Usage.InputTokens != 50 {
		t.Errorf("inputTokens = %d, want 50", last.Usage.InputTokens)
	}
}

// --- helpers ---

func joinLines(lines ...string) string {
	return strings.Join(lines, "\n")
}

func collectEvents(t *testing.T, tr *EventTranslator) []Event {
	t.Helper()
	var events []Event
	for tr.Next() {
		events = append(events, tr.Event())
	}
	if tr.Err() != nil {
		t.Fatalf("translator error: %v", tr.Err())
	}
	return events
}

type errorReader struct{ err error }

func (r *errorReader) Read(_ []byte) (int, error) { return 0, r.err }

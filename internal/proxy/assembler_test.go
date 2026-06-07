package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bermudi/cmd-code-proxy/internal/api"
)

// The assembler tests are organized one test per event class. A protocol fix
// (e.g. a new field on tool-input-delta) should only need to touch the case
// in assembler.go and the corresponding test below — not the integration
// tests in handler_test.go.

// --- streaming: per-event-class coverage ---------------------------------

func TestStreamAssembler_TextDelta(t *testing.T) {
	run := feedStream(t, []string{
		`{"type":"text-delta","text":"hi"}`,
		`{"type":"finish","finishReason":"stop"}`,
	})
	assertDeltaContent(t, run, "hi")
	assertFinishReason(t, run, "stop")
}

func TestStreamAssembler_ReasoningDelta(t *testing.T) {
	run := feedStream(t, []string{
		`{"type":"reasoning-delta","text":"think"}`,
		`{"type":"finish","finishReason":"stop"}`,
	})
	assertDeltaReasoning(t, run, "think")
}

func TestStreamAssembler_ToolUseAndDelta(t *testing.T) {
	run := feedStream(t, []string{
		`{"type":"tool-use","toolCallId":"c1","toolName":"fn"}`,
		`{"type":"tool-delta","text":"a"}`,
		`{"type":"tool-delta","text":"b"}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	})
	assertToolCall(t, run, 0, "c1", "fn", "ab")
	assertFinishReason(t, run, "tool_calls")
}

func TestStreamAssembler_ToolInputStartAndDelta(t *testing.T) {
	run := feedStream(t, []string{
		`{"type":"tool-input-start","id":"c1","toolName":"fn"}`,
		`{"type":"tool-input-delta","id":"c1","delta":"{}"}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	})
	assertToolCall(t, run, 0, "c1", "fn", "{}")
	assertFinishReason(t, run, "tool_calls")
}

func TestStreamAssembler_ToolInputDeltaWithoutStartPromotes(t *testing.T) {
	// Defensive: a delta arriving without a prior start should not be
	// dropped. We promote it to its own slot.
	run := feedStream(t, []string{
		`{"type":"tool-input-delta","id":"orphan","delta":"data"}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	})
	// Streaming deltas carry index+args but not id (per OpenAI spec);
	// the id is sent on the start chunk. The point of this test is the
	// args are emitted rather than dropped.
	if !hasToolCall(run, 0, "", "data") {
		t.Errorf("expected tool[0] args=data, chunks=%v", run.chunks)
	}
}

func TestStreamAssembler_ToolCallEmitsOnce(t *testing.T) {
	// tool-call arriving after tool-input-start should NOT double-emit.
	run := feedStream(t, []string{
		`{"type":"tool-input-start","id":"c1","toolName":"fn"}`,
		`{"type":"tool-input-delta","id":"c1","delta":"{}"}`,
		`{"type":"tool-call","toolCallId":"c1","toolName":"fn","input":{}}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	})
	// Only one tool call should be visible across the chunks.
	count := 0
	for _, line := range run.chunks {
		if line == "[DONE]" {
			continue
		}
		var r api.OpenAIChatResponse
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if len(r.Choices) == 0 || r.Choices[0].Delta == nil {
			continue
		}
		count += len(r.Choices[0].Delta.ToolCalls)
	}
	if count != 2 {
		// 1 for the start, 1 for the args delta. tool-call is suppressed.
		t.Errorf("tool-call chunks = %d, want 2 (start + args only)", count)
	}
}

func TestStreamAssembler_MultipleToolCallsShareRegistry(t *testing.T) {
	run := feedStream(t, []string{
		`{"type":"tool-input-start","id":"a","toolName":"fn1"}`,
		`{"type":"tool-input-delta","id":"a","delta":"1"}`,
		`{"type":"tool-input-start","id":"b","toolName":"fn2"}`,
		`{"type":"tool-input-delta","id":"b","delta":"2"}`,
		`{"type":"tool-input-delta","id":"a","delta":"3"}`, // back to a
		`{"type":"finish","finishReason":"tool-calls"}`,
	})
	if !hasToolCall(run, 0, "fn1", "13") {
		t.Errorf("expected tool[0] = fn1/13, chunks=%v", run.chunks)
	}
	if !hasToolCall(run, 1, "fn2", "2") {
		t.Errorf("expected tool[1] = fn2/2, chunks=%v", run.chunks)
	}
}

func TestStreamAssembler_RoleEmittedOnce(t *testing.T) {
	run := feedStream(t, []string{
		`{"type":"text-delta","text":"a"}`,
		`{"type":"text-delta","text":"b"}`,
		`{"type":"reasoning-delta","text":"r"}`,
		`{"type":"finish","finishReason":"stop"}`,
	})
	roleCount := 0
	for _, line := range run.chunks {
		if line == "[DONE]" {
			continue
		}
		var r api.OpenAIChatResponse
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if len(r.Choices) > 0 && r.Choices[0].Delta != nil && r.Choices[0].Delta.Role != "" {
			roleCount++
		}
	}
	if roleCount != 1 {
		t.Errorf("role emitted %d times, want 1", roleCount)
	}
}

func TestStreamAssembler_FinishReasonNormalized(t *testing.T) {
	run := feedStream(t, []string{
		`{"type":"text-delta","text":"x"}`,
		`{"type":"finish","finishReason":"max-tokens"}`,
	})
	assertFinishReason(t, run, "length")
}

func TestStreamAssembler_ContextCancelStopsLoop(t *testing.T) {
	// Endless stream that should be cut short by ctx cancel.
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`{"type":"text-delta","text":"a"}` + "\n"))
		// Never write a finish; the assembler must stop on ctx cancel.
		_ = pw.Close()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run starts; no events should be emitted.

	rec := httptest.NewRecorder()
	a := NewStreamAssembler(&Proxy{Debug: false}, rec, rec, "id", "m", time.Now().Unix())
	_ = a.Run(ctx, pr)

	if rec.Body.Len() != 0 {
		t.Errorf("expected no output after cancel, got %q", rec.Body.String())
	}
}

// --- non-streaming: per-event-class coverage -----------------------------

func TestFinalAssembler_TextDelta(t *testing.T) {
	r := feedFinal(t, []string{
		`{"type":"text-delta","text":"hello"}`,
		`{"type":"finish","finishReason":"stop"}`,
	})
	if c, _ := r.Choices[0].Message.Content.(string); c != "hello" {
		t.Errorf("Content = %v, want hello", r.Choices[0].Message.Content)
	}
}

func TestFinalAssembler_FinishReasonNormalized(t *testing.T) {
	// The non-streaming path used to hard-code "stop" and drop upstream's
	// real reason. This is the regression test for that contradiction.
	r := feedFinal(t, []string{
		`{"type":"text-delta","text":"x"}`,
		`{"type":"finish","finishReason":"max-tokens"}`,
	})
	if r.Choices[0].FinishReason == nil || *r.Choices[0].FinishReason != "length" {
		t.Errorf("FinishReason = %v, want length", r.Choices[0].FinishReason)
	}
}

func TestFinalAssembler_ToolCallsArgsConcatenated(t *testing.T) {
	r := feedFinal(t, []string{
		`{"type":"tool-input-start","id":"c1","toolName":"fn"}`,
		`{"type":"tool-input-delta","id":"c1","delta":"a"}`,
		`{"type":"tool-input-delta","id":"c1","delta":"b"}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	})
	msg := r.Choices[0].Message
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "c1" || tc.Function.Name != "fn" || tc.Function.Arguments != "ab" {
		t.Errorf("tool call = %+v, want id=c1 name=fn args=ab", tc)
	}
	if r.Choices[0].FinishReason == nil || *r.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %v, want tool_calls", r.Choices[0].FinishReason)
	}
}

func TestFinalAssembler_ToolCallUpdatesExistingSlot(t *testing.T) {
	// tool-call arriving after tool-input-start should update the existing
	// slot's name + args rather than appending a duplicate.
	r := feedFinal(t, []string{
		`{"type":"tool-input-start","id":"c1","toolName":""}`,
		`{"type":"tool-input-delta","id":"c1","delta":"partial"}`,
		`{"type":"tool-call","toolCallId":"c1","toolName":"fn","input":{"k":"v"}}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	})
	msg := r.Choices[0].Message
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.Function.Name != "fn" {
		t.Errorf("Name = %q, want fn", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"k":"v"}` {
		t.Errorf("Arguments = %q, want canonical input", tc.Function.Arguments)
	}
}

func TestFinalAssembler_DropsDeadToolInputBuffers(t *testing.T) {
	// Regression: the legacy code held a toolInputBuffers map that was
	// written but never read. Sanity-check that the unified assembler
	// still produces the same final args without it.
	r := feedFinal(t, []string{
		`{"type":"tool-input-start","id":"c1","toolName":"fn"}`,
		`{"type":"tool-input-delta","id":"c1","delta":"{\"a\":"}`,
		`{"type":"tool-input-delta","id":"c1","delta":"1}"}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	})
	if got := r.Choices[0].Message.ToolCalls[0].Function.Arguments; got != `{"a":1}` {
		t.Errorf("Arguments = %q, want %q", got, `{"a":1}`)
	}
}

func TestFinalAssembler_ToolDeltaBeforeAnyToolIsDropped(t *testing.T) {
	// A bare tool-delta with no prior tool-start should not fabricate a
	// tool call nor crash. The registry's lastIndex sentinel of -1 keeps
	// us from indexing into an empty toolCalls slice.
	r := feedFinal(t, []string{
		`{"type":"tool-delta","text":"orphan args"}`,
		`{"type":"text-delta","text":"hi"}`,
		`{"type":"finish","finishReason":"stop"}`,
	})
	if len(r.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want empty", r.Choices[0].Message.ToolCalls)
	}
	if c, _ := r.Choices[0].Message.Content.(string); c != "hi" {
		t.Errorf("Content = %v, want hi", r.Choices[0].Message.Content)
	}
}

func TestFinalAssembler_OrphanToolInputDeltaIsDropped(t *testing.T) {
	// Non-streaming contract: a delta with no matching start is dropped
	// rather than fabricating a tool call. (Streaming promotes the
	// orphan; see TestStreamAssembler_ToolInputDeltaWithoutStartPromotes.)
	r := feedFinal(t, []string{
		`{"type":"tool-input-delta","id":"orphan","delta":"data"}`,
		`{"type":"finish","finishReason":"stop"}`,
	})
	if len(r.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want empty", r.Choices[0].Message.ToolCalls)
	}
	if r.Choices[0].FinishReason == nil || *r.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason = %v, want stop", r.Choices[0].FinishReason)
	}
}

func TestFinalAssembler_UsagePopulated(t *testing.T) {
	r := feedFinal(t, []string{
		`{"type":"text-delta","text":"x"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":4,"outputTokens":2,"cachedInputTokens":1}}`,
	})
	if r.Usage == nil {
		t.Fatal("Usage is nil")
	}
	if r.Usage.PromptTokens != 3 || r.Usage.CompletionTokens != 2 || r.Usage.TotalTokens != 6 {
		t.Errorf("Usage = %+v, want prompt=3 completion=2 total=6", r.Usage)
	}
	if r.Usage.CacheReadTokens != 1 {
		t.Errorf("CacheReadTokens = %d, want 1", r.Usage.CacheReadTokens)
	}
}

func TestFinalAssembler_UsageDisjoint(t *testing.T) {
	// Upstream inputTokens=5320 includes 5234 cached. OpenAI convention: prompt_tokens
	// should be the non-cached portion (86), cache_read_tokens reported separately.
	r := feedFinal(t, []string{
		`{"type":"text-delta","text":"x"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5320,"outputTokens":42,"cachedInputTokens":5234}}`,
	})
	if r.Usage == nil {
		t.Fatal("Usage is nil")
	}
	if r.Usage.PromptTokens != 86 {
		t.Errorf("PromptTokens = %d, want 86 (5320 - 5234)", r.Usage.PromptTokens)
	}
	if r.Usage.CompletionTokens != 42 {
		t.Errorf("CompletionTokens = %d, want 42", r.Usage.CompletionTokens)
	}
	if r.Usage.CacheReadTokens != 5234 {
		t.Errorf("CacheReadTokens = %d, want 5234", r.Usage.CacheReadTokens)
	}
	expectedTotal := 86 + 42 + 5234 // disjoint sum
	if r.Usage.TotalTokens != expectedTotal {
		t.Errorf("TotalTokens = %d, want %d", r.Usage.TotalTokens, expectedTotal)
	}
}

// --- helpers --------------------------------------------------------------

type streamRun struct {
	chunks []string
}

func feedStream(t *testing.T, lines []string) streamRun {
	t.Helper()
	body := io.NopCloser(strings.NewReader(strings.Join(lines, "\n")))
	rec := httptest.NewRecorder()
	a := NewStreamAssembler(&Proxy{Debug: false}, rec, rec, "chatcmpl-test", "m", time.Now().Unix())
	if err := a.Run(context.Background(), body); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, _ := io.ReadAll(rec.Result().Body)
	return streamRun{chunks: extractSSEData(string(data))}
}

func feedFinal(t *testing.T, lines []string) api.OpenAIChatResponse {
	t.Helper()
	body := io.NopCloser(strings.NewReader(strings.Join(lines, "\n")))
	rec := httptest.NewRecorder()
	a := NewFinalAssembler(&Proxy{Debug: false}, rec, "chatcmpl-test", "m", time.Now().Unix())
	if err := a.Run(context.Background(), body); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var r api.OpenAIChatResponse
	if err := json.NewDecoder(rec.Result().Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return r
}

func assertDeltaContent(t *testing.T, run streamRun, want string) {
	t.Helper()
	got := concatDeltas(run, func(d api.OpenAIDelta) string { return d.Content })
	if got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

func assertDeltaReasoning(t *testing.T, run streamRun, want string) {
	t.Helper()
	got := concatDeltas(run, func(d api.OpenAIDelta) string { return d.ReasoningContent })
	if got != want {
		t.Errorf("reasoning = %q, want %q", got, want)
	}
}

func assertFinishReason(t *testing.T, run streamRun, want string) {
	t.Helper()
	for _, line := range run.chunks {
		if line == "[DONE]" {
			continue
		}
		var r api.OpenAIChatResponse
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Choices[0].FinishReason != nil {
			if *r.Choices[0].FinishReason != want {
				t.Errorf("FinishReason = %q, want %q", *r.Choices[0].FinishReason, want)
			}
			return
		}
	}
	t.Errorf("no finish_reason chunk in %v", run.chunks)
}

func assertToolCall(t *testing.T, run streamRun, idx int, wantID, wantName, wantArgs string) {
	t.Helper()
	gotID, gotName, gotArgs := toolCallAt(run, idx)
	if gotID != wantID || gotName != wantName || gotArgs != wantArgs {
		t.Errorf("tool[%d] = (id=%q name=%q args=%q), want (id=%q name=%q args=%q)",
			idx, gotID, gotName, gotArgs, wantID, wantName, wantArgs)
	}
}

func hasToolCall(run streamRun, idx int, wantName, wantArgs string) bool {
	_, name, args := toolCallAt(run, idx)
	return name == wantName && args == wantArgs
}

func toolCallAt(run streamRun, idx int) (id, name, argsOut string) {
	// Walk chunks in order, applying the deltas as an OpenAI client would.
	names := map[int]string{}
	argBufs := map[int]string{}
	ids := map[int]string{}
	for _, line := range run.chunks {
		if line == "[DONE]" {
			continue
		}
		var r api.OpenAIChatResponse
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if len(r.Choices) == 0 || r.Choices[0].Delta == nil {
			continue
		}
		for _, tc := range r.Choices[0].Delta.ToolCalls {
			if tc.ID != "" {
				ids[tc.Index] = tc.ID
			}
			if tc.Function != nil {
				if tc.Function.Name != "" {
					names[tc.Index] = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					argBufs[tc.Index] += tc.Function.Arguments
				}
			}
		}
	}
	return ids[idx], names[idx], argBufs[idx]
}

func concatDeltas(run streamRun, pick func(api.OpenAIDelta) string) string {
	var out string
	for _, line := range run.chunks {
		if line == "[DONE]" {
			continue
		}
		var r api.OpenAIChatResponse
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if len(r.Choices) == 0 || r.Choices[0].Delta == nil {
			continue
		}
		out += pick(*r.Choices[0].Delta)
	}
	return out
}

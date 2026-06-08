// Package paritytest compares the current ResponseAssembler against a
// verbatim copy of the pre-refactor StreamResponse / NonStreamResponse.
//
// The old code is vendored here as plain free functions (no *Proxy
// receiver) so the test can run both implementations side by side on the
// same NDJSON fixture. If the byte-level outputs differ for any fixture,
// the refactor has changed observable behavior.
package paritytest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/bermudi/cmd-code-proxy/internal/api"
	"github.com/bermudi/cmd-code-proxy/internal/proxy"
)

// --- vendored old code (verbatim from HEAD:internal/proxy/proxy.go) -------
//
// Adapted only to drop the *Proxy receiver and to make helpers plain
// functions. The event-handling logic is byte-identical to the pre-refactor
// implementation.

// oldNormalizeFinishReason — verbatim from old proxy.go.
func oldNormalizeFinishReason(reason string) string {
	switch reason {
	case "tool_calls", "tool-calls":
		return "tool_calls"
	case "length", "max_tokens", "max_output_tokens", "max-tokens":
		return "length"
	case "content_filter", "content-filter":
		return "content_filter"
	default:
		return "stop"
	}
}

// oldWriteSSE — verbatim.
func oldWriteSSE(w io.Writer, flusher http.Flusher, resp api.OpenAIChatResponse) {
	data, _ := json.Marshal(resp)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// oldStreamResponse — verbatim, minus *Proxy receiver and debugf call.
func oldStreamResponse(w http.ResponseWriter, r *http.Request, body io.ReadCloser, requestID, model string, created int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	t := newOldEventTranslator(body)
	sentRole := false
	toolCallIndex := 0
	toolCallIndexes := map[string]int{}

	for t.Next() {
		select {
		case <-r.Context().Done():
			return
		default:
		}

		event := t.Event()

		switch event.Type {
		case oldEventTextDelta:
			delta := api.OpenAIDelta{Content: event.Text}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			oldWriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case oldEventReasoningStart:
			// no-op

		case oldEventReasoningDelta:
			delta := api.OpenAIDelta{ReasoningContent: event.Text}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			oldWriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case oldEventReasoningEnd:
			// no-op

		case oldEventToolResult:
			// no-op

		case oldEventToolUse:
			toolCalls := []api.OpenAIDeltaToolCall{{
				Index:    toolCallIndex,
				ID:       event.ToolCallID,
				Type:     "function",
				Function: &api.OpenAIDeltaFunction{Name: event.ToolName},
			}}
			delta := api.OpenAIDelta{ToolCalls: toolCalls}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			oldWriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})
			toolCallIndex++

		case oldEventToolDelta:
			toolCalls := []api.OpenAIDeltaToolCall{{
				Index:    toolCallIndex - 1,
				Function: &api.OpenAIDeltaFunction{Arguments: event.Text},
			}}
			oldWriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &api.OpenAIDelta{ToolCalls: toolCalls}}},
			})

		case oldEventToolInputStart:
			if _, ok := toolCallIndexes[event.ID]; !ok {
				toolCallIndexes[event.ID] = toolCallIndex
				toolCallIndex++
			}
			delta := api.OpenAIDelta{ToolCalls: []api.OpenAIDeltaToolCall{{
				Index: toolCallIndexes[event.ID],
				ID:    event.ID,
				Type:  "function",
				Function: &api.OpenAIDeltaFunction{
					Name: event.ToolName,
				},
			}}}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			oldWriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case oldEventToolInputDelta:
			idx, ok := toolCallIndexes[event.ID]
			if !ok {
				idx = toolCallIndex
				toolCallIndexes[event.ID] = idx
				toolCallIndex++
			}
			oldWriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &api.OpenAIDelta{ToolCalls: []api.OpenAIDeltaToolCall{{
					Index:    idx,
					Function: &api.OpenAIDeltaFunction{Arguments: event.Delta},
				}}}}},
			})

		case oldEventToolCall:
			if _, alreadyStreamed := toolCallIndexes[event.ToolCallID]; alreadyStreamed {
				continue
			}
			idx := toolCallIndex
			toolCallIndexes[event.ToolCallID] = idx
			toolCallIndex++
			args := ""
			if event.Input != nil {
				if data, err := json.Marshal(event.Input); err == nil {
					args = string(data)
				}
			}
			delta := api.OpenAIDelta{ToolCalls: []api.OpenAIDeltaToolCall{{
				Index: idx,
				ID:    event.ToolCallID,
				Type:  "function",
				Function: &api.OpenAIDeltaFunction{
					Name:      event.ToolName,
					Arguments: args,
				},
			}}}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			oldWriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case oldEventFinish:
			reason := oldNormalizeFinishReason(event.FinishReason)
			oldWriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{
					Index:        0,
					Delta:        &api.OpenAIDelta{},
					FinishReason: &reason,
				}},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()

		case oldEventError:
			// logged in original; skipped here for parity
		}
	}
}

// oldNonStreamResponse — verbatim, minus *Proxy receiver and debugf call.
func oldNonStreamResponse(w http.ResponseWriter, body io.ReadCloser, requestID, model string, created int64) {
	t := newOldEventTranslator(body)

	var content strings.Builder
	var reasoningContent strings.Builder
	var inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int
	var hasToolCalls bool
	var toolCalls []api.ToolCall
	toolCallByID := map[string]int{}
	toolInputBuffers := map[string]*strings.Builder{}

	for t.Next() {
		event := t.Event()

		switch event.Type {
		case oldEventTextDelta:
			content.WriteString(event.Text)
		case oldEventReasoningStart:
			// no-op
		case oldEventReasoningDelta:
			reasoningContent.WriteString(event.Text)
		case oldEventReasoningEnd:
			// no-op
		case oldEventToolUse:
			hasToolCalls = true
			toolCallByID[event.ToolCallID] = len(toolCalls)
			toolCalls = append(toolCalls, api.ToolCall{
				ID:   event.ToolCallID,
				Type: "function",
				Function: api.FunctionCall{
					Name:      event.ToolName,
					Arguments: "",
				},
			})
		case oldEventToolDelta:
			if len(toolCalls) > 0 {
				toolCalls[len(toolCalls)-1].Function.Arguments += event.Text
			}
		case oldEventToolInputStart:
			hasToolCalls = true
			toolCallByID[event.ID] = len(toolCalls)
			toolInputBuffers[event.ID] = &strings.Builder{}
			toolCalls = append(toolCalls, api.ToolCall{
				ID:   event.ID,
				Type: "function",
				Function: api.FunctionCall{
					Name:      event.ToolName,
					Arguments: "",
				},
			})
		case oldEventToolInputDelta:
			if b := toolInputBuffers[event.ID]; b != nil {
				b.WriteString(event.Delta)
			}
			if idx, ok := toolCallByID[event.ID]; ok {
				toolCalls[idx].Function.Arguments += event.Delta
			}
		case oldEventToolCall:
			hasToolCalls = true
			args := ""
			if event.Input != nil {
				if data, err := json.Marshal(event.Input); err == nil {
					args = string(data)
				}
			}
			if idx, ok := toolCallByID[event.ToolCallID]; ok {
				toolCalls[idx].Function.Name = event.ToolName
				if args != "" {
					toolCalls[idx].Function.Arguments = args
				}
			} else {
				toolCallByID[event.ToolCallID] = len(toolCalls)
				toolCalls = append(toolCalls, api.ToolCall{
					ID:   event.ToolCallID,
					Type: "function",
					Function: api.FunctionCall{
						Name:      event.ToolName,
						Arguments: args,
					},
				})
			}
		case oldEventToolResult:
			// no-op
		case oldEventFinish:
			if event.Usage != nil {
				inputTokens = event.Usage.InputTokens
				outputTokens = event.Usage.OutputTokens
				cacheReadTokens = event.Usage.CacheReadInputTokens
				cacheWriteTokens = event.Usage.CacheCreationInputTokens
			}
		case oldEventError:
			// logged in original; skipped here for parity
		}
	}

	msg := &api.OpenAIMessage{
		Role:    "assistant",
		Content: content.String(),
	}
	if reasoningContent.Len() > 0 {
		msg.ReasoningContent = reasoningContent.String()
	}
	finishReason := "stop"
	if hasToolCalls {
		msg.Content = nil
		msg.ToolCalls = toolCalls
		finishReason = "tool_calls"
	}

	usage := &api.OpenAIUsage{
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      inputTokens + outputTokens,
	}
	if cacheReadTokens > 0 {
		usage.CacheReadTokens = cacheReadTokens
	}
	if cacheWriteTokens > 0 {
		usage.CacheWriteTokens = cacheWriteTokens
	}

	response := api.OpenAIChatResponse{
		ID:      requestID,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []api.OpenAIChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finishReason,
		}},
		Usage: usage,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// --- vendored old EventTranslator -----------------------------------------
//
// Verbatim copy of internal/proxy/translator.go from HEAD, with types
// renamed to old* to avoid collision when the paritytest package imports
// the current proxy package.

type oldEventType int

const (
	oldEventTextDelta oldEventType = iota
	oldEventReasoningStart
	oldEventReasoningDelta
	oldEventReasoningEnd
	oldEventToolResult
	oldEventToolUse
	oldEventToolDelta
	oldEventToolInputStart
	oldEventToolInputDelta
	oldEventToolCall
	oldEventFinish
	oldEventError
)

type oldEvent struct {
	Type         oldEventType
	RawLine      string
	Text         string
	ID           string
	ToolCallID   string
	ToolName     string
	Delta        string
	Input        map[string]any
	FinishReason string
	Usage        *oldEventUsage
	Error        *oldStreamError
}

type oldEventUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
}

type oldStreamError struct {
	Message    string
	StatusCode *int
}

type oldEventTranslator struct {
	scanner *bufio.Scanner
	event   oldEvent
	err     error
}

func newOldEventTranslator(r io.Reader) *oldEventTranslator {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	return &oldEventTranslator{scanner: s}
}

func (t *oldEventTranslator) Next() bool {
	for t.scanner.Scan() {
		line := strings.TrimSpace(t.scanner.Text())
		if line == "" {
			continue
		}

		var raw api.CCStreamEvent
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}

		t.event = oldEvent{RawLine: line}
		switch raw.Type {
		case "text-delta":
			t.event.Type = oldEventTextDelta
			t.event.Text = raw.Text
		case "reasoning-start":
			t.event.Type = oldEventReasoningStart
		case "reasoning-delta":
			t.event.Type = oldEventReasoningDelta
			t.event.Text = raw.Text
		case "reasoning-end":
			t.event.Type = oldEventReasoningEnd
		case "tool-result":
			t.event.Type = oldEventToolResult
		case "tool-use":
			t.event.Type = oldEventToolUse
			t.event.ToolCallID = raw.ToolCallID
			t.event.ToolName = raw.ToolName
		case "tool-delta":
			t.event.Type = oldEventToolDelta
			t.event.Text = raw.Text
		case "tool-input-start":
			t.event.Type = oldEventToolInputStart
			t.event.ID = raw.ID
			t.event.ToolName = raw.ToolName
		case "tool-input-delta":
			t.event.Type = oldEventToolInputDelta
			t.event.ID = raw.ID
			t.event.Delta = raw.Delta
		case "tool-call":
			t.event.Type = oldEventToolCall
			t.event.ToolCallID = raw.ToolCallID
			t.event.ToolName = raw.ToolName
			t.event.Input = raw.Input
		case "finish":
			t.event.Type = oldEventFinish
			t.event.FinishReason = raw.FinishReason
			if raw.TotalUsage != nil {
				t.event.Usage = &oldEventUsage{
					InputTokens:              raw.TotalUsage.InputTokens,
					OutputTokens:             raw.TotalUsage.OutputTokens,
					CacheReadInputTokens:     raw.TotalUsage.CacheReadInputTokens,
					CacheCreationInputTokens: raw.TotalUsage.CacheCreationInputTokens,
				}
			}
		case "error":
			t.event.Type = oldEventError
			if raw.Error != nil {
				t.event.Error = &oldStreamError{
					Message:    raw.Error.Message,
					StatusCode: raw.Error.StatusCode,
				}
			}
		default:
			continue
		}
		return true
	}
	t.err = t.scanner.Err()
	return false
}

func (t *oldEventTranslator) Event() oldEvent { return t.event }
func (t *oldEventTranslator) Err() error      { return t.err }

// --- parity harness -------------------------------------------------------

// runOldStream feeds ndjson through the OLD streaming code; returns the
// raw bytes the writer produced (header + body).
func runOldStream(t *testing.T, ndjson string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	body := io.NopCloser(strings.NewReader(ndjson))
	oldStreamResponse(rec, req, body, "chatcmpl-test", "test-model", 1700000000)
	return rec.Body.Bytes()
}

// runNewStream feeds the same ndjson through the NEW assembler.
func runNewStream(t *testing.T, ndjson string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	a := proxy.NewStreamAssembler(context.Background(), &proxy.Proxy{}, rec, rec, "chatcmpl-test", "test-model", 1700000000)
	body := io.NopCloser(strings.NewReader(ndjson))
	if err := a.Run(context.Background(), body); err != nil {
		t.Fatalf("new Run: %v", err)
	}
	return rec.Body.Bytes()
}

// runOldFinal feeds ndjson through the OLD non-streaming code.
func runOldFinal(t *testing.T, ndjson string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	body := io.NopCloser(strings.NewReader(ndjson))
	oldNonStreamResponse(rec, body, "chatcmpl-test", "test-model", 1700000000)
	return rec.Body.Bytes()
}

// runNewFinal feeds the same ndjson through the NEW final assembler.
func runNewFinal(t *testing.T, ndjson string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	a := proxy.NewFinalAssembler(context.Background(), &proxy.Proxy{}, rec, "chatcmpl-test", "test-model", 1700000000)
	body := io.NopCloser(strings.NewReader(ndjson))
	if err := a.Run(context.Background(), body); err != nil {
		t.Fatalf("new Run: %v", err)
	}
	return rec.Body.Bytes()
}

// normalizeForParity massages both outputs to ignore the bits the
// refactor *intentionally* changed:
//   - request_id contains a UUID (constant here, fine)
//   - debug-log lines are stripped from neither (we never emit them to
//     the response writer — both old and new use p.debugf to stderr)
//   - finishReason: the NEW non-streaming path propagates upstream's
//     normalized reason; the OLD non-streaming path synthesized "stop"
//     or "tool_calls". That's the fix the problem called out, so we
//     don't normalize it away — we want to SEE the difference.
//
// Returns the canonical JSON encoding of the SSE chunks (deduped [DONE]
// entries are stripped) and the final response, so differences are
// structural, not whitespace/formatting artifacts.
func normalizeStreamForParity(raw []byte) string {
	// Extract the data: ... lines from SSE, dropping [DONE].
	var chunks []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		// Re-marshal each chunk via a stable intermediate so field
		// ordering doesn't cause false diffs.
		var v any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			chunks = append(chunks, payload)
			continue
		}
		b, _ := json.Marshal(v)
		chunks = append(chunks, string(b))
	}
	return strings.Join(chunks, "\n")
}

func normalizeFinalForParity(raw []byte) string {
	// Re-marshal so field ordering is canonical.
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	// The finish_reason and content/tool_calls split is the only
	// intentional difference. We surface it by re-marshalling.
	b, _ := json.Marshal(v)
	return string(b)
}

// diffBytes returns a compact human-readable diff if old != new.
func diffBytes(label string, oldB, newB []byte) string {
	if bytes.Equal(oldB, newB) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n=== %s DIFF ===\n", label)
	fmt.Fprintf(&b, "--- old (pre-refactor) ---\n%s\n", string(oldB))
	fmt.Fprintf(&b, "--- new (post-refactor) ---\n%s\n", string(newB))
	return b.String()
}

// parityFixtures enumerates CommandCode NDJSON streams to feed through
// both the old (pre-refactor) and new (assembler) implementations.
//
// Each fixture declares an `expected` map from a JSON path to a
// constraint on the *new* value. Constraints are compared with
// applyConstraint(). A path not in the map must produce the *same*
// value in both old and new code (otherwise the test fails with a
// regression report). A path in the map is allowed to differ, but
// only in the specified way.
//
// This catches both:
//   - accidental regressions (a path the fixture didn't expect to
//     change, did change), and
//   - silent-value regressions (a path the fixture expected to be a
//     specific new value, became a different new value).
type parityFixture struct {
	name     string
	ndjson   string
	expected map[string]constraint
}

// constraint is a predicate over the *new* value at a path. nil means
// "value may be anything, just don't match the old code exactly."
// typedConstraint helpers below build concrete ones.
type constraint func(newVal any) bool

// valueIs allows the new value to be exactly v.
func valueIs(v any) constraint {
	return func(nv any) bool { return reflect.DeepEqual(nv, v) }
}

// valueAbsent allows the new value to be missing entirely (the field
// is omitted from the JSON).
func valueAbsent() constraint {
	return func(nv any) bool { return nv == nil }
}

// valuePresent means the new value may be anything, but it must exist
// (the field must be in the response body).
func valuePresent() constraint {
	return func(nv any) bool { return nv != nil }
}

var parityFixtures = []parityFixture{
	{
		name: "text_only",
		ndjson: strings.Join([]string{
			`{"type":"text-delta","text":"Hello "}`,
			`{"type":"text-delta","text":"world"}`,
			`{"type":"finish","finishReason":"stop"}`,
		}, "\n"),
		// old: usage:{0,0,0}  new: usage absent
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("stop"),
		},
	},
	{
		name: "reasoning_then_text",
		ndjson: strings.Join([]string{
			`{"type":"reasoning-start"}`,
			`{"type":"reasoning-delta","text":"think"}`,
			`{"type":"reasoning-end"}`,
			`{"type":"text-delta","text":"answer"}`,
			`{"type":"finish","finishReason":"stop"}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("stop"),
		},
	},
	{
		name: "tool_use_with_deltas",
		ndjson: strings.Join([]string{
			`{"type":"tool-use","toolCallId":"c1","toolName":"get_weather"}`,
			`{"type":"tool-delta","text":"{\"city\":"}`,
			`{"type":"tool-delta","text":"\"SF\"}"}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("tool_calls"),
		},
	},
	{
		name: "tool_input_start_with_deltas",
		ndjson: strings.Join([]string{
			`{"type":"tool-input-start","id":"c1","toolName":"fn"}`,
			`{"type":"tool-input-delta","id":"c1","delta":"{\"a\":"}`,
			`{"type":"tool-input-delta","id":"c1","delta":"1}"}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("tool_calls"),
		},
	},
	{
		name: "tool_call_with_input",
		ndjson: strings.Join([]string{
			`{"type":"tool-call","toolCallId":"c1","toolName":"fn","input":{"k":"v"}}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("tool_calls"),
		},
	},
	{
		name: "tool_input_then_tool_call",
		ndjson: strings.Join([]string{
			`{"type":"tool-input-start","id":"c1","toolName":""}`,
			`{"type":"tool-input-delta","id":"c1","delta":"partial"}`,
			`{"type":"tool-call","toolCallId":"c1","toolName":"fn","input":{"k":"v"}}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("tool_calls"),
		},
	},
	{
		name: "two_parallel_tools",
		ndjson: strings.Join([]string{
			`{"type":"tool-input-start","id":"a","toolName":"fn1"}`,
			`{"type":"tool-input-delta","id":"a","delta":"1"}`,
			`{"type":"tool-input-start","id":"b","toolName":"fn2"}`,
			`{"type":"tool-input-delta","id":"b","delta":"2"}`,
			`{"type":"tool-input-delta","id":"a","delta":"3"}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("tool_calls"),
		},
	},
	{
		name: "tool_result_interleaved",
		ndjson: strings.Join([]string{
			`{"type":"text-delta","text":"calling tool"}`,
			`{"type":"tool-use","toolCallId":"c1","toolName":"fn"}`,
			`{"type":"tool-result"}`,
			`{"type":"text-delta","text":"done"}`,
			`{"type":"finish","finishReason":"stop"}`,
		}, "\n"),
		// finish_reason must be "stop" (upstream said stop). The old
		// code hard-coded "tool_calls" because hasToolCalls was true.
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("stop"),
		},
	},
	{
		name:   "empty_stream",
		ndjson: ``,
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("stop"),
		},
	},
	{
		name: "finish_with_usage",
		ndjson: strings.Join([]string{
			`{"type":"text-delta","text":"x"}`,
			`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":3,"cachedInputTokens":1,"cacheCreationInputTokens":2}}`,
		}, "\n"),
		// Old code dropped usage in streaming; new code emits it on the
		// final chunk. Intentional improvement — lets clients (like Pi)
		// report token counts.
		//
		// prompt_tokens changed from 10 (total including cached) to 7
		// (non-cached only: 10 - 1 cacheRead - 2 cacheWrite = 7).
		// This is an intentional fix: OpenAI convention is that
		// prompt_tokens and cache_read_tokens are disjoint. The old code
		// double-counted cached tokens.
		expected: map[string]constraint{
			"usage":                   valuePresent(),
			"usage.prompt_tokens":     valueIs(float64(7)),
			"usage.cache_read_tokens": valueIs(float64(1)),
			"usage.cache_write_tokens": valueIs(float64(2)),
		},
	},
	{
		name: "finish_length",
		ndjson: strings.Join([]string{
			`{"type":"text-delta","text":"x"}`,
			`{"type":"finish","finishReason":"max-tokens"}`,
		}, "\n"),
		// finish_reason must be "length" (old hard-coded "stop").
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("length"),
		},
	},
	{
		name: "finish_content_filter",
		ndjson: strings.Join([]string{
			`{"type":"text-delta","text":"x"}`,
			`{"type":"finish","finishReason":"content-filter"}`,
		}, "\n"),
		// finish_reason must be "content_filter" (old hard-coded "stop").
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("content_filter"),
		},
	},
	{
		// Realistic CommandCode stream: reasoning preamble, text
		// answer, no tool calls. The streaming version must emit
		// the role exactly once (on the first delta).
		name: "realistic_text_only",
		ndjson: strings.Join([]string{
			`{"type":"reasoning-start"}`,
			`{"type":"reasoning-delta","text":"The user is asking for a haiku. "}`,
			`{"type":"reasoning-delta","text":"Five-seven-five. "}`,
			`{"type":"reasoning-delta","text":"Topic: mountains. "}`,
			`{"type":"reasoning-end"}`,
			`{"type":"text-delta","text":"Mountains rise,"}`,
			`{"type":"text-delta","text":" mist crawls through the pines —"}`,
			`{"type":"text-delta","text":" silence."}`,
			`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":12,"outputTokens":7}}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valuePresent(),
			"finish_reason": valueIs("stop"),
		},
	},
	{
		// Realistic CommandCode stream: tool call, tool result,
		// then text answer. The tool call should be one entry with
		// the full args.
		name: "realistic_tool_then_text",
		ndjson: strings.Join([]string{
			`{"type":"text-delta","text":"Let me check."}`,
			`{"type":"tool-input-start","id":"call_abc","toolName":"get_weather"}`,
			`{"type":"tool-input-delta","id":"call_abc","delta":"{\"city\":"}`,
			`{"type":"tool-input-delta","id":"call_abc","delta":"\"San Francisco\"}"}`,
			`{"type":"tool-result","toolCallId":"call_abc"}`,
			`{"type":"text-delta","text":"It's 14°C and foggy."}`,
			`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":20,"outputTokens":12}}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valuePresent(),
			"finish_reason": valueIs("stop"),
		},
	},
	{
		// Interleaved reasoning and tool: the new code should
		// handle the same interleaving the old code did.
		name: "interleaved_reasoning_and_tool",
		ndjson: strings.Join([]string{
			`{"type":"reasoning-delta","text":"I'll need to call a tool. "}`,
			`{"type":"tool-use","toolCallId":"c1","toolName":"search"}`,
			`{"type":"tool-delta","text":"{\"q\":"}`,
			`{"type":"tool-delta","text":"\"weather SF\"}"}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("tool_calls"),
		},
	},
	{
		// Multiple tool-calls in a single response. Indices must
		// be 0 and 1, never colliding.
		name: "three_serial_tools",
		ndjson: strings.Join([]string{
			`{"type":"tool-input-start","id":"a","toolName":"fn1"}`,
			`{"type":"tool-input-delta","id":"a","delta":"1"}`,
			`{"type":"tool-input-start","id":"b","toolName":"fn2"}`,
			`{"type":"tool-input-delta","id":"b","delta":"2"}`,
			`{"type":"tool-input-start","id":"c","toolName":"fn3"}`,
			`{"type":"tool-input-delta","id":"c","delta":"3"}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("tool_calls"),
		},
	},
	{
		// tool-call event arrives with full input BEFORE any
		// tool-input-start. The new code must register the id
		// and emit the canonical args. The old code did the same.
		name: "tool_call_only",
		ndjson: strings.Join([]string{
			`{"type":"tool-call","toolCallId":"c1","toolName":"fn","input":{"x":1}}`,
			`{"type":"finish","finishReason":"tool-calls"}`,
		}, "\n"),
		expected: map[string]constraint{
			"usage":         valueAbsent(),
			"finish_reason": valueIs("tool_calls"),
		},
	},
}

// --- tests ----------------------------------------------------------------

func TestParity_Stream(t *testing.T) {
	for _, fx := range parityFixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			oldOut := normalizeStreamForParity(runOldStream(t, fx.ndjson))
			newOut := normalizeStreamForParity(runNewStream(t, fx.ndjson))
			if oldOut == newOut {
				return
			}

			// Byte-exact match failed. Parse chunks pairwise and apply
			// the same constraint system as the final test.
			oldChunks := parseStreamChunks(t, runOldStream(t, fx.ndjson))
			newChunks := parseStreamChunks(t, runNewStream(t, fx.ndjson))

			if len(oldChunks) != len(newChunks) {
				oldRaw := runOldStream(t, fx.ndjson)
				newRaw := runNewStream(t, fx.ndjson)
				t.Errorf("stream output differs for %s: chunk count %d (old) vs %d (new)\n%s", fx.name, len(oldChunks), len(newChunks), diffBytes(fx.name, oldRaw, newRaw))
				return
			}

			var unexpected []string
			for i := range oldChunks {
				diffs := diffJSON("", oldChunks[i], newChunks[i])
				for _, d := range diffs {
					oldVal, newVal := lookupPath(oldChunks[i], d), lookupPath(newChunks[i], d)
					con, ok := lookupConstraint(d, fx.expected)
					if !ok {
						unexpected = append(unexpected, fmt.Sprintf("chunk[%d] %s: %v (old) -> %v (new) — no constraint declared", i, d, oldVal, newVal))
						continue
					}
					if !con(newVal) {
						unexpected = append(unexpected, fmt.Sprintf("chunk[%d] %s: new value %v fails constraint", i, d, newVal))
					}
				}
			}
			if len(unexpected) > 0 {
				oldRaw := runOldStream(t, fx.ndjson)
				newRaw := runNewStream(t, fx.ndjson)
				t.Errorf("stream output differs for %s\n%s\nunexpected diffs:\n  %s", fx.name, diffBytes(fx.name, oldRaw, newRaw), strings.Join(unexpected, "\n  "))
			}
		})
	}
}

// parseStreamChunks extracts and parses each data: line from SSE into a
// map[string]any. [DONE] lines are skipped.
func parseStreamChunks(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatalf("invalid chunk JSON: %s: %v", payload, err)
		}
		chunks = append(chunks, v)
	}
	return chunks
}

func TestParity_Final(t *testing.T) {
	for _, fx := range parityFixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			oldRaw := runOldFinal(t, fx.ndjson)
			newRaw := runNewFinal(t, fx.ndjson)
			oldDoc := parseJSONObject(t, oldRaw)
			newDoc := parseJSONObject(t, newRaw)

			// Walk every leaf path in newDoc and check that either
			// the values match (good) or the new value satisfies the
			// constraint for that path (also good). If a path has
			// no constraint and the values differ, that's a
			// regression.
			//
			// We also walk oldDoc paths that are absent in newDoc —
			// those are removals, which are allowed only if a
			// constraint exists for that path.
			diffs := diffJSON("", oldDoc, newDoc)
			if len(diffs) == 0 {
				return
			}

			var unexpected []string
			for _, d := range diffs {
				oldVal, newVal := lookupPath(oldDoc, d), lookupPath(newDoc, d)
				con, ok := lookupConstraint(d, fx.expected)
				if !ok {
					unexpected = append(unexpected, fmt.Sprintf("%s: %v (old) -> %v (new) — no constraint declared", d, oldVal, newVal))
					continue
				}
				if !con(newVal) {
					unexpected = append(unexpected, fmt.Sprintf("%s: new value %v fails constraint", d, newVal))
				}
			}
			if len(unexpected) > 0 {
				t.Errorf("final output has UNEXPECTED diffs for %s:\n  %s\n%s", fx.name, strings.Join(unexpected, "\n  "), diffBytes(fx.name, oldRaw, newRaw))
			} else {
				t.Logf("final output for %s has only expected diffs: %v", fx.name, diffs)
			}
		})
	}
}

// lookupPath returns the value at dot-separated path in doc, or nil
// if the path is absent. Works for nested maps and array indices.
func lookupPath(doc map[string]any, path string) any {
	if doc == nil {
		return nil
	}
	if path == "" {
		return doc
	}
	parts := strings.Split(path, ".")
	var cur any = doc
	for _, p := range parts {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[p]
		case []any:
			idx := 0
			fmt.Sscanf(p, "%d", &idx)
			if idx < 0 || idx >= len(c) {
				return nil
			}
			cur = c[idx]
		default:
			return nil
		}
	}
	return cur
}

// lookupConstraint returns the constraint whose key matches the
// longest suffix of path, or (nil, false) if no constraint applies.
func lookupConstraint(path string, constraints map[string]constraint) (constraint, bool) {
	for k, c := range constraints {
		if k == path {
			return c, true
		}
		if strings.HasSuffix(path, "."+k) {
			return c, true
		}
	}
	return nil, false
}

// parseJSONObject decodes the OpenAIChatResponse envelope for diffing.
// Returns nil if the body is empty (e.g. the request was never
// finalized).
func parseJSONObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return v
}

// diffJSON returns the dot-paths where old and new differ. The paths
// are coarse (e.g. "usage", "choices.0.finish_reason") — enough to
// classify a regression vs. an intentional improvement.
func diffJSON(path string, oldV, newV any) []string {
	if reflect.DeepEqual(oldV, newV) {
		return nil
	}
	switch o := oldV.(type) {
	case map[string]any:
		n, ok := newV.(map[string]any)
		if !ok {
			return []string{path}
		}
		var keys []string
		for k := range o {
			keys = append(keys, k)
		}
		for k := range n {
			keys = append(keys, k)
		}
		seen := map[string]bool{}
		var out []string
		for _, k := range keys {
			if seen[k] {
				continue
			}
			seen[k] = true
			sub := path
			if sub != "" {
				sub += "."
			}
			out = append(out, diffJSON(sub+k, o[k], n[k])...)
		}
		return out
	case []any:
		n, ok := newV.([]any)
		if !ok || len(o) != len(n) {
			return []string{path}
		}
		var out []string
		for i := range o {
			out = append(out, diffJSON(fmt.Sprintf("%s.%d", path, i), o[i], n[i])...)
		}
		return out
	default:
		return []string{path}
	}
}

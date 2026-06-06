package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/bermudi/cmd-code-proxy/internal/api"
)

// ResponseAssembler owns the per-event dispatch that turns a CommandCode NDJSON
// stream into an OpenAI-shaped response. It exists so that streaming and
// non-streaming handlers share one switch, one tool-index registry, and one
// finish-reason policy — instead of two switches that drift on protocol fixes.
//
// Construct with NewStreamAssembler (writes SSE as events arrive) or
// NewFinalAssembler (buffers and emits a single JSON response). The constructor
// flag picks the mode; the event dispatch is identical.
type ResponseAssembler struct {
	p       *Proxy
	w       http.ResponseWriter
	flusher http.Flusher // nil for non-streaming
	requestID,
	model string
	created   int64
	streaming bool

	// State shared across event handlers.
	sentRole     bool
	toolRegistry toolIndexRegistry
	finishReason string
	usage        *api.OpenAIUsage

	// Non-streaming accumulation.
	contentBuilder          strings.Builder
	reasoningContentBuilder strings.Builder
	toolCalls               []api.ToolCall
	hasToolCalls            bool
}

// NewStreamAssembler returns an assembler that writes OpenAI SSE chunks as
// each event arrives. The caller must have already set the response headers
// (Content-Type: text/event-stream, etc.) and obtained a Flusher from w.
func NewStreamAssembler(p *Proxy, w http.ResponseWriter, flusher http.Flusher, requestID, model string, created int64) *ResponseAssembler {
	return &ResponseAssembler{
		p:            p,
		w:            w,
		flusher:      flusher,
		requestID:    requestID,
		model:        model,
		created:      created,
		streaming:    true,
		toolRegistry: newToolIndexRegistry(),
	}
}

// NewFinalAssembler returns an assembler that buffers the entire stream and
// emits a single OpenAI chat-completion JSON object on completion.
func NewFinalAssembler(p *Proxy, w http.ResponseWriter, requestID, model string, created int64) *ResponseAssembler {
	return &ResponseAssembler{
		p:            p,
		w:            w,
		requestID:    requestID,
		model:        model,
		created:      created,
		toolRegistry: newToolIndexRegistry(),
	}
}

// Run drives the dispatcher over the given translator. It returns the first
// I/O or write error encountered, or nil on a clean finish. Context
// cancellation aborts the loop early (used by streaming to drop the client).
func (a *ResponseAssembler) Run(ctx context.Context, body io.ReadCloser) error {
	t := NewEventTranslator(body)
	for t.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}

		event := t.Event()
		a.p.debugf("[DEBUG] CommandCode stream line: %s", truncateLog(event.RawLine))
		a.handle(event)
	}
	if err := t.Err(); err != nil && err != io.EOF {
		log.Printf("[ERROR] Scanner error: %v", err)
		return err
	}

	if !a.streaming {
		return a.writeFinal()
	}
	return nil
}

// handle is the single switch. Each case is a one-line dispatch to the mode
// methods below; the per-mode work lives in the strategy methods, so a
// protocol change touches one case body and the relevant strategy method —
// not two adjacent code blocks.
func (a *ResponseAssembler) handle(event Event) {
	switch event.Type {
	case EventTextDelta:
		a.onTextDelta(event.Text)

	case EventReasoningStart, EventReasoningEnd, EventToolResult:
		// Informational; no output in either mode.

	case EventReasoningDelta:
		a.onReasoningDelta(event.Text)

	case EventToolUse:
		a.onToolStart(event.ToolCallID, event.ToolName, "", nil)

	case EventToolDelta:
		// No id; extends whatever the most-recently-started tool was.
		idx := a.toolRegistry.lastIndex()
		if idx < 0 {
			log.Printf("[WARN] tool-delta with no preceding tool event")
			return
		}
		a.onToolArgs(idx, event.Text, false /* not canonical */)

	case EventToolInputStart:
		a.onToolStart(event.ID, event.ToolName, "", nil)

	case EventToolInputDelta:
		idx, ok := a.toolRegistry.lookup(event.ID)
		if !ok {
			// Defensive: a delta arrived without a matching start. The
			// upstream contract guarantees a start precedes deltas, so this
			// is malformed input. We log and drop in non-streaming mode;
			// in streaming we still emit the delta (at a fresh index) so
			// the client at least sees the data — matching the legacy
			// behavior of producing a single args chunk with no id.
			log.Printf("[WARN] tool-input-delta for unknown id %q", event.ID)
			if !a.streaming {
				return
			}
			idx = a.toolRegistry.register(event.ID)
			a.onToolArgs(idx, event.Delta, false)
			return
		}
		a.onToolArgs(idx, event.Delta, false)

	case EventToolCall:
		args := ""
		if event.Input != nil {
			if data, err := json.Marshal(event.Input); err == nil {
				args = string(data)
			}
		}
		if _, ok := a.toolRegistry.lookup(event.ToolCallID); ok {
			// Id already seen via tool-input-start / tool-use. The
			// canonical args replace the streamed fragments; the
			// canonical name fills in if missing. Streaming suppresses
			// emission entirely (the client already saw the chunks).
			a.onToolCallEnrich(event.ToolCallID, event.ToolName, args)
			return
		}
		a.onToolStart(event.ToolCallID, event.ToolName, args, event.Input)

	case EventFinish:
		a.onFinish(event)

	case EventError:
		log.Printf("[ERROR] Stream error: %v", event.Error)
	}
}

// --- per-event mode strategies -------------------------------------------
//
// Each strategy branches on streaming vs. buffered in exactly one place —
// the methods below. The cases in handle() are mode-agnostic, so adding a
// new mode (e.g. "tee" — write both) is a matter of implementing these
// methods on a new type, not editing every case.

func (a *ResponseAssembler) onTextDelta(text string) {
	if a.streaming {
		a.streamText(text)
		return
	}
	a.contentBuilder.WriteString(text)
}

func (a *ResponseAssembler) onReasoningDelta(text string) {
	if a.streaming {
		a.streamReasoning(text)
		return
	}
	a.reasoningContentBuilder.WriteString(text)
}

// onToolStart registers the tool (or reuses its existing index) and emits
// the start-of-tool event. args is non-empty only when input was provided
// inline (the tool-call event path).
func (a *ResponseAssembler) onToolStart(idKey, name, args string, _ map[string]any) {
	idx := a.toolRegistry.register(idKey)
	if a.streaming {
		a.streamToolStart(idx, idKey, name, args)
		return
	}
	a.hasToolCalls = true
	a.toolCalls = append(a.toolCalls, api.ToolCall{
		ID:   idKey,
		Type: "function",
		Function: api.FunctionCall{
			Name:      name,
			Arguments: args,
		},
	})
}

// onToolArgs appends an arguments fragment to the given tool. canonical
// is true when the args came from a tool-call event (replacing, not
// appending); it's currently unused in non-streaming code paths because
// we only call onToolCallEnrich from the seen-via-lookup branch.
func (a *ResponseAssembler) onToolArgs(idx int, args string, _ bool) {
	if a.streaming {
		a.streamToolArgs(idx, args)
		return
	}
	if idx >= 0 && idx < len(a.toolCalls) {
		a.toolCalls[idx].Function.Arguments += args
	}
}

// onToolCallEnrich is invoked when a tool-call event arrives for an id
// already known to the registry. Streaming suppresses emission (the
// client already saw the chunks); non-streaming updates the slot.
func (a *ResponseAssembler) onToolCallEnrich(idKey, name, args string) {
	idx, ok := a.toolRegistry.lookup(idKey)
	if !ok {
		return
	}
	if a.streaming {
		return
	}
	if name != "" {
		a.toolCalls[idx].Function.Name = name
	}
	if args != "" {
		a.toolCalls[idx].Function.Arguments = args
	}
}

func (a *ResponseAssembler) onFinish(event Event) {
	a.finishReason = normalizeFinishReason(event.FinishReason)
	if event.Usage != nil {
		a.usage = usageFromEvent(event.Usage)
	}
	if a.streaming {
		a.streamFinish()
	}
}

// --- streaming emitters ---------------------------------------------------

func (a *ResponseAssembler) baseChunk() api.OpenAIChatResponse {
	return api.OpenAIChatResponse{
		ID:      a.requestID,
		Object:  "chat.completion.chunk",
		Created: a.created,
		Model:   a.model,
		Choices: []api.OpenAIChoice{{Index: 0}},
	}
}

func (a *ResponseAssembler) streamText(text string) {
	a.emitWithRole(api.OpenAIDelta{Content: text})
}

func (a *ResponseAssembler) streamReasoning(text string) {
	a.emitWithRole(api.OpenAIDelta{ReasoningContent: text})
}

func (a *ResponseAssembler) emitWithRole(delta api.OpenAIDelta) {
	if !a.sentRole {
		delta.Role = "assistant"
		a.sentRole = true
	}
	chunk := a.baseChunk()
	chunk.Choices[0].Delta = &delta
	a.writeSSE(chunk)
}

func (a *ResponseAssembler) streamToolStart(idx int, id, name, args string) {
	a.emitWithRole(api.OpenAIDelta{
		ToolCalls: []api.OpenAIDeltaToolCall{{
			Index:    idx,
			ID:       id,
			Type:     "function",
			Function: &api.OpenAIDeltaFunction{Name: name, Arguments: args},
		}},
	})
}

func (a *ResponseAssembler) streamToolArgs(idx int, args string) {
	chunk := a.baseChunk()
	chunk.Choices[0].Delta = &api.OpenAIDelta{
		ToolCalls: []api.OpenAIDeltaToolCall{{
			Index:    idx,
			Function: &api.OpenAIDeltaFunction{Arguments: args},
		}},
	}
	a.writeSSE(chunk)
}

func (a *ResponseAssembler) streamFinish() {
	reason := a.finishReason
	chunk := a.baseChunk()
	chunk.Choices[0].Delta = &api.OpenAIDelta{}
	chunk.Choices[0].FinishReason = &reason
	a.writeSSE(chunk)
	fmt.Fprintf(a.w, "data: [DONE]\n\n")
	a.flusher.Flush()
}

func (a *ResponseAssembler) writeSSE(resp api.OpenAIChatResponse) {
	data, _ := json.Marshal(resp)
	fmt.Fprintf(a.w, "data: %s\n\n", data)
	a.flusher.Flush()
}

// --- non-streaming finalization ------------------------------------------

func (a *ResponseAssembler) writeFinal() error {
	msg := &api.OpenAIMessage{
		Role:    "assistant",
		Content: a.contentBuilder.String(),
	}
	if a.reasoningContentBuilder.Len() > 0 {
		msg.ReasoningContent = a.reasoningContentBuilder.String()
	}

	// If upstream signaled a finish reason, that wins. Otherwise fall back
	// to inference (tool_calls if any tool was emitted, else stop) so
	// streams that never receive a finish event still terminate correctly.
	finishReason := a.finishReason
	if finishReason == "" {
		if a.hasToolCalls {
			finishReason = "tool_calls"
		} else {
			finishReason = "stop"
		}
	}
	if a.hasToolCalls {
		msg.Content = nil
		msg.ToolCalls = a.toolCalls
	}

	w := a.w
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(api.OpenAIChatResponse{
		ID:      a.requestID,
		Object:  "chat.completion",
		Created: a.created,
		Model:   a.model,
		Choices: []api.OpenAIChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finishReason,
		}},
		Usage: a.usage,
	})
}

// --- tool-index registry --------------------------------------------------

// toolIndexRegistry gives each distinct tool id-key a stable index for the
// lifetime of one response. Streaming and non-streaming consumers need the
// same id→index mapping; the streaming side emits an OpenAI index per chunk,
// the non-streaming side mutates toolCalls[idx] in place.
type toolIndexRegistry struct {
	indexes map[string]int
	nextIdx int
	lastIdx int
}

func newToolIndexRegistry() toolIndexRegistry {
	return toolIndexRegistry{indexes: map[string]int{}, lastIdx: -1}
}

// register returns the index for idKey, allocating a new slot on first sight.
func (r *toolIndexRegistry) register(idKey string) int {
	if idx, ok := r.indexes[idKey]; ok {
		return idx
	}
	idx := r.nextIdx
	r.indexes[idKey] = idx
	r.nextIdx++
	r.lastIdx = idx
	return idx
}

// lookup returns the index previously assigned to idKey, if any.
func (r *toolIndexRegistry) lookup(idKey string) (int, bool) {
	idx, ok := r.indexes[idKey]
	return idx, ok
}

// lastIndex returns the index of the most recently allocated slot, or -1.
func (r *toolIndexRegistry) lastIndex() int {
	return r.lastIdx
}

// --- finish-reason policy + usage ----------------------------------------

// normalizeFinishReason maps CommandCode / OpenAI-shaped reasons to the
// canonical set the OpenAI spec promises clients: stop, length, tool_calls,
// content_filter. Unknown reasons default to "stop" so clients always get
// something the spec defines.
func normalizeFinishReason(reason string) string {
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

// usageFromEvent converts the translator's per-event usage view into the
// OpenAI wire shape, omitting cache fields when zero.
func usageFromEvent(u *EventUsage) *api.OpenAIUsage {
	usage := &api.OpenAIUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadInputTokens > 0 {
		usage.CacheReadTokens = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		usage.CacheWriteTokens = u.CacheCreationInputTokens
	}
	return usage
}

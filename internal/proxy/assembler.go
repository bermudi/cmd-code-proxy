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

// sink receives mode-specific side effects from the assembler's event
// dispatch. streamSink writes SSE chunks as they arrive; finalSink
// buffers and emits one JSON response at the end.
type sink interface {
	textDelta(text string)
	reasoningDelta(text string)
	toolStart(idx int, id, name, args string)
	toolArgs(idx int, args string)
	toolCallEnrich(idx int, name, args string)
	finish(reason string, usage *api.OpenAIUsage)
	finalize() error
}

// ResponseAssembler owns the per-event dispatch that turns a CommandCode NDJSON
// stream into an OpenAI-shaped response. It drives the EventTranslator loop
// and delegates all output to a sink — streamSink for SSE, finalSink for a
// single JSON body.
type ResponseAssembler struct {
	p              *Proxy
	toolRegistry   toolIndexRegistry
	sink           sink
	promoteOrphans bool // true for streaming, false for final
}

// NewStreamAssembler returns an assembler that writes OpenAI SSE chunks as
// each event arrives. The caller must have already set the response headers
// (Content-Type: text/event-stream, etc.) and obtained a Flusher from w.
func NewStreamAssembler(p *Proxy, w http.ResponseWriter, flusher http.Flusher, requestID, model string, created int64) *ResponseAssembler {
	return &ResponseAssembler{
		p:              p,
		toolRegistry:   newToolIndexRegistry(),
		sink:           newStreamSink(w, flusher, requestID, model, created),
		promoteOrphans: true,
	}
}

// NewFinalAssembler returns an assembler that buffers the entire stream and
// emits a single OpenAI chat-completion JSON object on completion.
func NewFinalAssembler(p *Proxy, w http.ResponseWriter, requestID, model string, created int64) *ResponseAssembler {
	return &ResponseAssembler{
		p:              p,
		toolRegistry:   newToolIndexRegistry(),
		sink:           newFinalSink(w, requestID, model, created),
		promoteOrphans: false,
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

	return a.sink.finalize()
}

// handle is the single switch. Each case resolves any registry lookups
// needed, then delegates to the sink. The sink never touches the registry.
func (a *ResponseAssembler) handle(event Event) {
	switch event.Type {
	case EventTextDelta:
		a.sink.textDelta(event.Text)

	case EventReasoningStart, EventReasoningEnd, EventToolResult:
		// Informational; no output in either mode.

	case EventReasoningDelta:
		a.sink.reasoningDelta(event.Text)

	case EventToolUse:
		idx := a.toolRegistry.register(event.ToolCallID)
		a.sink.toolStart(idx, event.ToolCallID, event.ToolName, "")

	case EventToolDelta:
		// No id; extends whatever the most-recently-started tool was.
		idx := a.toolRegistry.lastIndex()
		if idx < 0 {
			log.Printf("[WARN] tool-delta with no preceding tool event")
			return
		}
		a.sink.toolArgs(idx, event.Text)

	case EventToolInputStart:
		idx := a.toolRegistry.register(event.ID)
		a.sink.toolStart(idx, event.ID, event.ToolName, "")

	case EventToolInputDelta:
		idx, ok := a.toolRegistry.lookup(event.ID)
		if !ok {
			// Defensive: a delta arrived without a matching start. The
			// upstream contract guarantees a start precedes deltas, so this
			// is malformed input. Streaming promotes the orphan to a fresh
			// slot so the client at least sees the data; non-streaming drops
			// it to avoid fabricating a phantom tool call.
			log.Printf("[WARN] tool-input-delta for unknown id %q", event.ID)
			if !a.promoteOrphans {
				return
			}
			idx = a.toolRegistry.register(event.ID)
			a.sink.toolArgs(idx, event.Delta)
			return
		}
		a.sink.toolArgs(idx, event.Delta)

	case EventToolCall:
		args := ""
		if event.Input != nil {
			if data, err := json.Marshal(event.Input); err == nil {
				args = string(data)
			}
		}
		if idx, ok := a.toolRegistry.lookup(event.ToolCallID); ok {
			// Id already seen via tool-input-start / tool-use. The
			// canonical args replace the streamed fragments; the
			// canonical name fills in if missing. Streaming suppresses
			// emission entirely (the client already saw the chunks).
			a.sink.toolCallEnrich(idx, event.ToolName, args)
			return
		}
		idx := a.toolRegistry.register(event.ToolCallID)
		a.sink.toolStart(idx, event.ToolCallID, event.ToolName, args)

	case EventFinish:
		reason := normalizeFinishReason(event.FinishReason)
		var usage *api.OpenAIUsage
		if event.Usage != nil {
			usage = usageFromEvent(event.Usage)
		}
		a.sink.finish(reason, usage)

	case EventError:
		log.Printf("[ERROR] Stream error: %v", event.Error)
	}
}

// --- streamSink: writes SSE chunks as events arrive -----------------------

type streamSink struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	requestID string
	model     string
	created   int64
	sentRole  bool
}

func newStreamSink(w http.ResponseWriter, flusher http.Flusher, requestID, model string, created int64) *streamSink {
	return &streamSink{w: w, flusher: flusher, requestID: requestID, model: model, created: created}
}

func (s *streamSink) textDelta(text string) {
	s.emitWithRole(api.OpenAIDelta{Content: text})
}

func (s *streamSink) reasoningDelta(text string) {
	s.emitWithRole(api.OpenAIDelta{ReasoningContent: text})
}

func (s *streamSink) toolStart(idx int, id, name, args string) {
	s.emitWithRole(api.OpenAIDelta{
		ToolCalls: []api.OpenAIDeltaToolCall{{
			Index:    idx,
			ID:       id,
			Type:     "function",
			Function: &api.OpenAIDeltaFunction{Name: name, Arguments: args},
		}},
	})
}

func (s *streamSink) toolArgs(idx int, args string) {
	chunk := s.baseChunk()
	chunk.Choices[0].Delta = &api.OpenAIDelta{
		ToolCalls: []api.OpenAIDeltaToolCall{{
			Index:    idx,
			Function: &api.OpenAIDeltaFunction{Arguments: args},
		}},
	}
	s.writeSSE(chunk)
}

func (s *streamSink) toolCallEnrich(_ int, _ string, _ string) {
	// Already streamed via tool-input-start + deltas; suppress duplicate.
}

func (s *streamSink) finish(reason string, usage *api.OpenAIUsage) {
	chunk := s.baseChunk()
	chunk.Choices[0].Delta = &api.OpenAIDelta{}
	chunk.Choices[0].FinishReason = &reason
	if usage != nil {
		chunk.Usage = usage
	}
	s.writeSSE(chunk)
	fmt.Fprintf(s.w, "data: [DONE]\n\n")
	s.flusher.Flush()
}

func (s *streamSink) finalize() error { return nil }

func (s *streamSink) emitWithRole(delta api.OpenAIDelta) {
	if !s.sentRole {
		delta.Role = "assistant"
		s.sentRole = true
	}
	chunk := s.baseChunk()
	chunk.Choices[0].Delta = &delta
	s.writeSSE(chunk)
}

func (s *streamSink) baseChunk() api.OpenAIChatResponse {
	return api.OpenAIChatResponse{
		ID:      s.requestID,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []api.OpenAIChoice{{Index: 0}},
	}
}

func (s *streamSink) writeSSE(resp api.OpenAIChatResponse) {
	data, _ := json.Marshal(resp)
	fmt.Fprintf(s.w, "data: %s\n\n", data)
	s.flusher.Flush()
}

// --- finalSink: buffers and emits one JSON response -----------------------

type finalSink struct {
	w                       http.ResponseWriter
	requestID               string
	model                   string
	created                 int64
	contentBuilder          strings.Builder
	reasoningContentBuilder strings.Builder
	toolCalls               []api.ToolCall
	hasToolCalls            bool
	finishReason            string
	usage                   *api.OpenAIUsage
}

func newFinalSink(w http.ResponseWriter, requestID, model string, created int64) *finalSink {
	return &finalSink{w: w, requestID: requestID, model: model, created: created}
}

func (s *finalSink) textDelta(text string) {
	s.contentBuilder.WriteString(text)
}

func (s *finalSink) reasoningDelta(text string) {
	s.reasoningContentBuilder.WriteString(text)
}

func (s *finalSink) toolStart(idx int, id, name, args string) {
	if idx != len(s.toolCalls) {
		log.Printf("[WARN] finalSink: toolStart idx=%d but len(toolCalls)=%d; sequential contract violated", idx, len(s.toolCalls))
	}
	s.hasToolCalls = true
	s.toolCalls = append(s.toolCalls, api.ToolCall{
		ID:   id,
		Type: "function",
		Function: api.FunctionCall{
			Name:      name,
			Arguments: args,
		},
	})
}

func (s *finalSink) toolArgs(idx int, args string) {
	if idx >= 0 && idx < len(s.toolCalls) {
		s.toolCalls[idx].Function.Arguments += args
	}
}

func (s *finalSink) toolCallEnrich(idx int, name, args string) {
	if idx < 0 || idx >= len(s.toolCalls) {
		return
	}
	if name != "" {
		s.toolCalls[idx].Function.Name = name
	}
	if args != "" {
		s.toolCalls[idx].Function.Arguments = args
	}
}

func (s *finalSink) finish(reason string, usage *api.OpenAIUsage) {
	s.finishReason = reason
	s.usage = usage
}

func (s *finalSink) finalize() error {
	msg := &api.OpenAIMessage{
		Role:    "assistant",
		Content: s.contentBuilder.String(),
	}
	if s.reasoningContentBuilder.Len() > 0 {
		msg.ReasoningContent = s.reasoningContentBuilder.String()
	}

	// If upstream signaled a finish reason, that wins. Otherwise fall back
	// to inference (tool_calls if any tool was emitted, else stop) so
	// streams that never receive a finish event still terminate correctly.
	finishReason := s.finishReason
	if finishReason == "" {
		if s.hasToolCalls {
			finishReason = "tool_calls"
		} else {
			finishReason = "stop"
		}
	}
	if s.hasToolCalls {
		msg.Content = nil
		msg.ToolCalls = s.toolCalls
	}

	s.w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(s.w).Encode(api.OpenAIChatResponse{
		ID:      s.requestID,
		Object:  "chat.completion",
		Created: s.created,
		Model:   s.model,
		Choices: []api.OpenAIChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finishReason,
		}},
		Usage: s.usage,
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
	// Upstream inputTokens is the TOTAL (cached + uncached), following the
	// Vercel AI SDK convention. OpenAI's convention is disjoint: prompt_tokens
	// excludes cached tokens, which are reported separately. Subtract to match.
	promptTokens := u.InputTokens - u.CacheReadInputTokens - u.CacheCreationInputTokens
	if promptTokens < 0 {
		promptTokens = 0
	}
	usage := &api.OpenAIUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      promptTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
	}
	if u.CacheReadInputTokens > 0 {
		usage.CacheReadTokens = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		usage.CacheWriteTokens = u.CacheCreationInputTokens
	}
	return usage
}

package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"github.com/bermudi/cmd-code-proxy/internal/api"
)

// EventType enumerates the NDJSON event types produced by the CommandCode stream.
type EventType int

const (
	EventTextDelta       EventType = iota
	EventReasoningStart
	EventReasoningDelta
	EventReasoningEnd
	EventToolResult
	EventToolUse
	EventToolDelta
	EventToolInputStart
	EventToolInputDelta
	EventToolCall
	EventFinish
	EventError
)

// Event is a typed, decoded CommandCode stream event.
// Only a subset of fields is meaningful for each Type —
// see the per-Type docs on the const block above.
type Event struct {
	Type         EventType
	RawLine      string // original NDJSON line for debug logging

	// Text carries: text-delta content, reasoning-delta content, tool-delta arguments
	Text string
	// ID carries: tool-input-start ID, tool-input-delta ID
	ID string
	// ToolCallID carries: tool-use ID, tool-call ID
	ToolCallID string
	// ToolName carries: tool-use name, tool-input-start name, tool-call name
	ToolName string
	// Delta carries: tool-input-delta argument fragment
	Delta string
	// Input carries: tool-call full input object
	Input map[string]any
	// FinishReason carries: finish reason string
	FinishReason string
	// Usage carries: finish usage stats (nil if absent)
	Usage *EventUsage
	// Error carries: error details (nil if absent)
	Error *StreamError
}

// EventUsage carries token usage from a finish event.
type EventUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
}

// StreamError carries error details from an error event.
type StreamError struct {
	Message    string
	StatusCode *int
}

// EventTranslator scans a CommandCode NDJSON stream and yields typed Events.
type EventTranslator struct {
	scanner *bufio.Scanner
	event   Event
	err     error
}

// NewEventTranslator creates a translator that reads NDJSON events from r.
func NewEventTranslator(r io.Reader) *EventTranslator {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	return &EventTranslator{scanner: s}
}

// Next advances to the next valid event, skipping blank lines and unparseable JSON.
// Returns false when the stream ends or on read error (check Err()).
func (t *EventTranslator) Next() bool {
	for t.scanner.Scan() {
		line := strings.TrimSpace(t.scanner.Text())
		if line == "" {
			continue
		}

		var raw api.CCStreamEvent
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}

		t.event = Event{RawLine: line}
		switch raw.Type {
		case "text-delta":
			t.event.Type = EventTextDelta
			t.event.Text = raw.Text
		case "reasoning-start":
			t.event.Type = EventReasoningStart
		case "reasoning-delta":
			t.event.Type = EventReasoningDelta
			t.event.Text = raw.Text
		case "reasoning-end":
			t.event.Type = EventReasoningEnd
		case "tool-result":
			t.event.Type = EventToolResult
		case "tool-use":
			t.event.Type = EventToolUse
			t.event.ToolCallID = raw.ToolCallID
			t.event.ToolName = raw.ToolName
		case "tool-delta":
			t.event.Type = EventToolDelta
			t.event.Text = raw.Text
		case "tool-input-start":
			t.event.Type = EventToolInputStart
			t.event.ID = raw.ID
			t.event.ToolName = raw.ToolName
		case "tool-input-delta":
			t.event.Type = EventToolInputDelta
			t.event.ID = raw.ID
			t.event.Delta = raw.Delta
		case "tool-call":
			t.event.Type = EventToolCall
			t.event.ToolCallID = raw.ToolCallID
			t.event.ToolName = raw.ToolName
			t.event.Input = raw.Input
		case "finish":
			t.event.Type = EventFinish
			t.event.FinishReason = raw.FinishReason
			if raw.TotalUsage != nil {
				t.event.Usage = &EventUsage{
					InputTokens:              raw.TotalUsage.InputTokens,
					OutputTokens:             raw.TotalUsage.OutputTokens,
					CacheReadInputTokens:     raw.TotalUsage.CacheReadInputTokens,
					CacheCreationInputTokens: raw.TotalUsage.CacheCreationInputTokens,
				}
			}
		case "error":
			t.event.Type = EventError
			if raw.Error != nil {
				t.event.Error = &StreamError{
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

// Event returns the most recent event decoded by Next.
func (t *EventTranslator) Event() Event {
	return t.event
}

// Err returns the first non-EOF error encountered during scanning.
func (t *EventTranslator) Err() error {
	return t.err
}

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

func TestStreamResponse_ReasoningDelta(t *testing.T) {
	p := &Proxy{Debug: false}

	// Simulate upstream NDJSON body
	lines := []string{
		`{"type":"reasoning-start"}`,
		`{"type":"reasoning-delta","text":"Let me think"}`,
		`{"type":"reasoning-delta","text":" about this."}`,
		`{"type":"reasoning-end"}`,
		`{"type":"text-delta","text":"Hello!"}`,
		`{"type":"finish","finishReason":"stop"}`,
	}
	body := io.NopCloser(strings.NewReader(strings.Join(lines, "\n")))
	ccResp := &http.Response{Body: body}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	p.StreamResponse(rec, req, ccResp, "chatcmpl-test", "test-model", time.Now().Unix())

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	data, _ := io.ReadAll(resp.Body)
	// SSE format: each line starts with "data: "
	chunks := extractSSEData(string(data))

	var reasoningContent string
	var content string
	for _, line := range chunks {
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
		delta := r.Choices[0].Delta
		reasoningContent += delta.ReasoningContent
		content += delta.Content
	}

	if reasoningContent != "Let me think about this." {
		t.Errorf("reasoningContent = %q, want %q", reasoningContent, "Let me think about this.")
	}
	if content != "Hello!" {
		t.Errorf("content = %q, want %q", content, "Hello!")
	}
}

func TestNonStreamResponse_ReasoningDelta(t *testing.T) {
	p := &Proxy{Debug: false}

	lines := []string{
		`{"type":"reasoning-delta","text":"Step 1"}`,
		`{"type":"reasoning-delta","text":" and 2"}`,
		`{"type":"text-delta","text":"The answer is 42"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":5}}`,
	}
	body := io.NopCloser(strings.NewReader(strings.Join(lines, "\n")))
	ccResp := &http.Response{Body: body}

	rec := httptest.NewRecorder()
	p.NonStreamResponse(rec, ccResp, "chatcmpl-test", "test-model", time.Now().Unix())

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var r api.OpenAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(r.Choices) == 0 {
		t.Fatal("no choices")
	}
	msg := r.Choices[0].Message
	if msg == nil {
		t.Fatal("no message")
	}
	if msg.ReasoningContent != "Step 1 and 2" {
		t.Errorf("ReasoningContent = %q, want %q", msg.ReasoningContent, "Step 1 and 2")
	}
	if content, ok := msg.Content.(string); !ok || content != "The answer is 42" {
		t.Errorf("Content = %v, want %q", msg.Content, "The answer is 42")
	}
}

func extractSSEData(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}

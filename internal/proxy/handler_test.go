package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bermudi/cmd-code-proxy/internal/api"
)

// fakeUpstream implements Upstream for testing.
type fakeUpstream struct {
	generateFn func(ctx context.Context, ccBody api.CCRequestBody, apiKey string, tasteLearning bool) (io.ReadCloser, error)
	modelsFn   func(ctx context.Context, apiKey string) ([]api.OpenAIModel, error)
}

func (f *fakeUpstream) Generate(ctx context.Context, ccBody api.CCRequestBody, apiKey string, tasteLearning bool) (io.ReadCloser, error) {
	if f.generateFn != nil {
		return f.generateFn(ctx, ccBody, apiKey, tasteLearning)
	}
	return nil, fmt.Errorf("fakeUpstream: Generate not configured")
}

func (f *fakeUpstream) FetchModels(ctx context.Context, apiKey string) ([]api.OpenAIModel, error) {
	if f.modelsFn != nil {
		return f.modelsFn(ctx, apiKey)
	}
	return nil, fmt.Errorf("fakeUpstream: FetchModels not configured")
}

func cannedNDJSON(lines []string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(strings.Join(lines, "\n")))
}

func TestHandleChatCompletions_StreamWithFakeUpstream(t *testing.T) {
	ndjson := []string{
		`{"type":"text-delta","text":"Hello "}`,
		`{"type":"text-delta","text":"world"}`,
		`{"type":"finish","finishReason":"stop"}`,
	}

	p := NewProxy("test-key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, apiKey string, _ bool) (io.ReadCloser, error) {
			if apiKey != "test-key" {
				t.Errorf("apiKey = %q, want %q", apiKey, "test-key")
			}
			return cannedNDJSON(ndjson), nil
		},
	})

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	data, _ := io.ReadAll(resp.Body)
	chunks := extractSSEData(string(data))

	var content string
	for _, line := range chunks {
		if line == "[DONE]" {
			continue
		}
		var r api.OpenAIChatResponse
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if len(r.Choices) > 0 && r.Choices[0].Delta != nil {
			content += r.Choices[0].Delta.Content
		}
	}

	if content != "Hello world" {
		t.Errorf("content = %q, want %q", content, "Hello world")
	}
}

func TestHandleChatCompletions_NonStreamWithFakeUpstream(t *testing.T) {
	ndjson := []string{
		`{"type":"text-delta","text":"42"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":3}}`,
	}

	p := NewProxy("test-key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, _ string, _ bool) (io.ReadCloser, error) {
			return cannedNDJSON(ndjson), nil
		},
	})

	body := `{"model":"test-model","messages":[{"role":"user","content":"answer"}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var r api.OpenAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if content, ok := r.Choices[0].Message.Content.(string); !ok || content != "42" {
		t.Errorf("content = %v, want %q", r.Choices[0].Message.Content, "42")
	}
	if r.Usage == nil || r.Usage.TotalTokens != 13 {
		t.Errorf("usage = %v, want total=13", r.Usage)
	}
}

func TestHandleChatCompletions_UpstreamError4xx(t *testing.T) {
	p := NewProxy("test-key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, _ string, _ bool) (io.ReadCloser, error) {
			return nil, &UpstreamError{StatusCode: 400, Body: "bad request from upstream"}
		},
	})

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandleChatCompletions_UpstreamError5xx(t *testing.T) {
	p := NewProxy("test-key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, _ string, _ bool) (io.ReadCloser, error) {
			return nil, &UpstreamError{StatusCode: 500, Body: "internal error"}
		},
	})

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", resp.StatusCode)
	}
}

func TestHandleChatCompletions_NoAPIKey(t *testing.T) {
	p := NewProxy("", &fakeUpstream{})

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestHandleChatCompletions_ClientAPIKeyOverridesDefault(t *testing.T) {
	ndjson := []string{`{"type":"text-delta","text":"ok"}`, `{"type":"finish","finishReason":"stop"}`}

	var capturedKey string
	p := NewProxy("default-key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, apiKey string, _ bool) (io.ReadCloser, error) {
			capturedKey = apiKey
			return cannedNDJSON(ndjson), nil
		},
	})

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-key")
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	if capturedKey != "client-key" {
		t.Errorf("apiKey = %q, want %q", capturedKey, "client-key")
	}
}

func TestHandleChatCompletions_UsesProxyWorkingDirOverride(t *testing.T) {
	ndjson := []string{`{"type":"text-delta","text":"ok"}`, `{"type":"finish","finishReason":"stop"}`}
	override := "/home/daniel/Documents/AgenticWiki"

	var capturedBody api.CCRequestBody
	p := NewProxy("test-key", &fakeUpstream{
		generateFn: func(_ context.Context, body api.CCRequestBody, _ string, _ bool) (io.ReadCloser, error) {
			capturedBody = body
			return cannedNDJSON(ndjson), nil
		},
	})
	p.WorkingDir = override

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	if capturedBody.Config.WorkingDir != override {
		t.Errorf("WorkingDir = %q, want %q", capturedBody.Config.WorkingDir, override)
	}
}

func TestHandleChatCompletions_MethodNotAllowed(t *testing.T) {
	p := NewProxy("key", &fakeUpstream{})
	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestHandleChatCompletions_EmptyMessages(t *testing.T) {
	p := NewProxy("key", &fakeUpstream{})
	body := `{"model":"test-model","messages":[]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// TestHandleChatCompletions_AllSystemMessages — a request whose only
// message is a system turn passes the empty-input check (length is 1)
// but produces 0 messages after DropSystemMessages. The proxy must 400
// here rather than forwarding an empty messages array to CommandCode.
func TestHandleChatCompletions_AllSystemMessages(t *testing.T) {
	p := NewProxy("key", &fakeUpstream{})
	body := `{"model":"test-model","messages":[{"role":"system","content":"just a system message"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for all-system request, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no user/assistant/tool messages") {
		t.Errorf("expected explanatory 400 body, got %q", rec.Body.String())
	}
}

func TestHandleModels_WithFakeUpstream(t *testing.T) {
	models := []api.OpenAIModel{
		{ID: "deepseek/deepseek-v4-pro", Object: "model", OwnedBy: "deepseek"},
		{ID: "claude-4-opus", Object: "model", OwnedBy: "anthropic"},
	}
	p := NewProxy("test-key", &fakeUpstream{
		modelsFn: func(_ context.Context, _ string) ([]api.OpenAIModel, error) {
			return models, nil
		},
	})

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	p.HandleModels(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var list api.OpenAIModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// ListClosedModels=false by default, should filter out claude-4-opus (no slash)
	if len(list.Data) != 1 || list.Data[0].ID != "deepseek/deepseek-v4-pro" {
		t.Errorf("models = %v, want only deepseek/deepseek-v4-pro", list.Data)
	}
}

func TestHandleModels_IncludeClosed(t *testing.T) {
	models := []api.OpenAIModel{
		{ID: "deepseek/deepseek-v4-pro", Object: "model", OwnedBy: "deepseek"},
		{ID: "claude-4-opus", Object: "model", OwnedBy: "anthropic"},
	}
	p := NewProxy("test-key", &fakeUpstream{
		modelsFn: func(_ context.Context, _ string) ([]api.OpenAIModel, error) {
			return models, nil
		},
	})
	p.ListClosedModels = true

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	p.HandleModels(rec, req)

	var list api.OpenAIModelList
	json.NewDecoder(rec.Result().Body).Decode(&list)
	if len(list.Data) != 2 {
		t.Errorf("expected 2 models, got %d", len(list.Data))
	}
}

func TestHandleModels_FallbackWhenUpstreamFails(t *testing.T) {
	p := NewProxy("test-key", &fakeUpstream{
		modelsFn: func(_ context.Context, _ string) ([]api.OpenAIModel, error) {
			return nil, io.ErrUnexpectedEOF
		},
	})

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	p.HandleModels(rec, req)

	var list api.OpenAIModelList
	json.NewDecoder(rec.Result().Body).Decode(&list)
	if len(list.Data) == 0 {
		t.Error("expected fallback models, got empty list")
	}
}

func TestBuildCCRequest_Defaults(t *testing.T) {
	req := api.OpenAIChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []api.OpenAIMessage{{Role: "user", Content: "hi"}},
	}

	ccBody, err := BuildCCRequest(req)
	if err != nil {
		t.Fatalf("BuildCCRequest: %v", err)
	}
	if ccBody.Params.Model != "deepseek/deepseek-v4-pro" {
		t.Errorf("model = %q, want %q", ccBody.Params.Model, "deepseek/deepseek-v4-pro")
	}
	if ccBody.Params.MaxTokens != 64000 {
		t.Errorf("maxTokens = %d, want 64000", ccBody.Params.MaxTokens)
	}
	if ccBody.Params.Stream != true {
		t.Error("stream should always be true")
	}
	if ccBody.PermissionMode != "auto-accept" {
		t.Errorf("permissionMode = %q, want %q", ccBody.PermissionMode, "auto-accept")
	}
	if ccBody.Skills != "" {
		t.Errorf("skills = %q, want empty string", ccBody.Skills)
	}
	if ccBody.ThreadID == nil || *ccBody.ThreadID == "" {
		t.Errorf("threadId = %v, want generated UUID", ccBody.ThreadID)
	}
	// Without client config, falls back to populateConfigFromFS — which impersonates command-code
	if !strings.Contains(ccBody.Config.Environment, "Node.js") {
		t.Errorf("environment = %q, want 'Node.js' substring (impersonates command-code CLI)", ccBody.Config.Environment)
	}
}

func TestBuildCCRequest_ClientConfig(t *testing.T) {
	clientCfg := &api.CCConfig{
		WorkingDir:    "/home/daniel/Documents/NewsWiki",
		Date:          "2026-06-08",
		Environment:   "linux-x64, Node.js v26.2.0",
		Structure:     []string{"meta", "raw", "scripts", "wiki"},
		IsGitRepo:     true,
		CurrentBranch: "main",
		MainBranch:    "main",
		GitStatus:     "Working tree clean",
		RecentCommits: []string{"abc1234 initial commit", "def5678 second commit", "ghi9012 third commit"},
	}
	req := api.OpenAIChatRequest{
		Model:              "MiniMaxAI/MiniMax-M3",
		Messages:           []api.OpenAIMessage{{Role: "user", Content: "hello"}},
		XCommandCodeConfig: clientCfg,
	}

	ccBody, err := BuildCCRequest(req)
	if err != nil {
		t.Fatalf("BuildCCRequest: %v", err)
	}

	// Client config should be used verbatim — no FS fallback
	if ccBody.Config.WorkingDir != "/home/daniel/Documents/NewsWiki" {
		t.Errorf("workingDir = %q, want client-provided value", ccBody.Config.WorkingDir)
	}
	if !ccBody.Config.IsGitRepo {
		t.Error("isGitRepo should be true from client config")
	}
	if ccBody.Config.CurrentBranch != "main" {
		t.Errorf("currentBranch = %q, want 'main'", ccBody.Config.CurrentBranch)
	}
	if len(ccBody.Config.RecentCommits) != 3 {
		t.Errorf("recentCommits = %v, want 3 entries", ccBody.Config.RecentCommits)
	}
	if ccBody.Config.Environment != "linux-x64, Node.js v26.2.0" {
		t.Errorf("environment = %q, want client-provided value", ccBody.Config.Environment)
	}
}

func TestHandleChatCompletions_ToolCallStreaming(t *testing.T) {
	ndjson := []string{
		`{"type":"text-delta","text":"I'll call a tool."}`,
		`{"type":"tool-input-start","id":"call_1","toolName":"get_weather"}`,
		`{"type":"tool-input-delta","id":"call_1","delta":"{\"city\":"}`,
		`{"type":"tool-input-delta","id":"call_1","delta":"\"SF\"}"}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	}

	p := NewProxy("key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, _ string, _ bool) (io.ReadCloser, error) {
			return cannedNDJSON(ndjson), nil
		},
	})

	body := `{"model":"test","messages":[{"role":"user","content":"weather?"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	resp := rec.Result()
	data, _ := io.ReadAll(resp.Body)
	chunks := extractSSEData(string(data))

	var textContent string
	var toolCallNames []string
	var toolCallArgs []string
	var finishReason string

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
		textContent += delta.Content
		for _, tc := range delta.ToolCalls {
			if tc.Function != nil {
				if tc.Function.Name != "" {
					toolCallNames = append(toolCallNames, tc.Function.Name)
				}
				if tc.Function.Arguments != "" {
					toolCallArgs = append(toolCallArgs, tc.Function.Arguments)
				}
			}
		}
		if r.Choices[0].FinishReason != nil {
			finishReason = *r.Choices[0].FinishReason
		}
	}

	if textContent != "I'll call a tool." {
		t.Errorf("text = %q, want %q", textContent, "I'll call a tool.")
	}
	if len(toolCallNames) != 1 || toolCallNames[0] != "get_weather" {
		t.Errorf("tool names = %v, want [get_weather]", toolCallNames)
	}
	combinedArgs := strings.Join(toolCallArgs, "")
	if combinedArgs != `{"city":"SF"}` {
		t.Errorf("tool args = %q, want %q", combinedArgs, `{"city":"SF"}`)
	}
	if finishReason != "tool_calls" {
		t.Errorf("finish = %q, want %q", finishReason, "tool_calls")
	}
}

func TestHandleChatCompletions_CaptureDir(t *testing.T) {
	ndjson := []string{
		`{"type":"text-delta","text":"Hello"}`,
		`{"type":"finish","finishReason":"stop"}`,
	}

	captureDir := t.TempDir()

	p := NewProxy("key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, _ string, _ bool) (io.ReadCloser, error) {
			return cannedNDJSON(ndjson), nil
		},
	})
	p.CaptureDir = captureDir

	body := `{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	// Verify the SSE response is still correct (capture is transparent).
	resp := rec.Result()
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), "Hello") {
		t.Errorf("response should contain Hello, got: %s", data)
	}

	// Verify both capture files were written.
	entries, err := os.ReadDir(captureDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 capture files (request + response), got %d", len(entries))
	}

	// Find the response file (*.ndjson).
	var respFile, reqFile string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ndjson") {
			respFile = filepath.Join(captureDir, e.Name())
		} else if strings.HasSuffix(e.Name(), ".request.json") {
			reqFile = filepath.Join(captureDir, e.Name())
		}
	}

	// Response capture must contain both NDJSON lines exactly.
	respBytes, err := os.ReadFile(respFile)
	if err != nil {
		t.Fatalf("ReadFile response: %v", err)
	}
	for _, line := range ndjson {
		if !strings.Contains(string(respBytes), line) {
			t.Errorf("response capture missing line %q", line)
		}
	}

	// Request capture must be valid JSON with the expected model.
	reqBytes, err := os.ReadFile(reqFile)
	if err != nil {
		t.Fatalf("ReadFile request: %v", err)
	}
	var reqBody map[string]any
	if err := json.Unmarshal(reqBytes, &reqBody); err != nil {
		t.Fatalf("request capture is not valid JSON: %v", err)
	}
	params, ok := reqBody["params"].(map[string]any)
	if !ok || params["model"] != "test" {
		t.Errorf("request capture model = %v, want test", params["model"])
	}
}

func TestHandleChatCompletions_TransportError(t *testing.T) {
	p := NewProxy("key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, _ string, _ bool) (io.ReadCloser, error) {
			return nil, io.ErrUnexpectedEOF
		},
	})

	body := `{"model":"test","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for transport error, got %d", rec.Code)
	}
}

// TestHandleChatCompletions_TasteLearningFromRequest — when the client
// sets x_command_code_taste_learning, that value reaches upstream
// (verified by capturing it via fakeUpstream).
func TestHandleChatCompletions_TasteLearningFromRequest(t *testing.T) {
	ndjson := []string{
		`{"type":"text-delta","text":"ok"}`,
		`{"type":"finish","finishReason":"stop"}`,
	}

	var capturedTaste bool
	p := NewProxy("test-key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, _ string, tasteLearning bool) (io.ReadCloser, error) {
			capturedTaste = tasteLearning
			return cannedNDJSON(ndjson), nil
		},
	})
	// Even with proxy default true, per-request false wins.
	tr := true
	p.TasteLearning = &tr

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true,"x_command_code_taste_learning":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	if capturedTaste != false {
		t.Errorf("tasteLearning = %v, want false (per-request override)", capturedTaste)
	}
}

// TestHandleChatCompletions_TasteLearningFromProxy — when the client
// doesn't set the field, the proxy default applies.
func TestHandleChatCompletions_TasteLearningFromProxy(t *testing.T) {
	ndjson := []string{
		`{"type":"text-delta","text":"ok"}`,
		`{"type":"finish","finishReason":"stop"}`,
	}

	var capturedTaste bool
	p := NewProxy("test-key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, _ string, tasteLearning bool) (io.ReadCloser, error) {
			capturedTaste = tasteLearning
			return cannedNDJSON(ndjson), nil
		},
	})
	fa := false
	p.TasteLearning = &fa

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	if capturedTaste != false {
		t.Errorf("tasteLearning = %v, want false (proxy default)", capturedTaste)
	}
}

// TestHandleChatCompletions_TasteLearningDefault — when neither the
// client nor the proxy sets a value, the binary default (true) wins.
// This is the backward-compat case for existing deployments.
func TestHandleChatCompletions_TasteLearningDefault(t *testing.T) {
	ndjson := []string{
		`{"type":"text-delta","text":"ok"}`,
		`{"type":"finish","finishReason":"stop"}`,
	}

	var capturedTaste bool
	p := NewProxy("test-key", &fakeUpstream{
		generateFn: func(_ context.Context, _ api.CCRequestBody, _ string, tasteLearning bool) (io.ReadCloser, error) {
			capturedTaste = tasteLearning
			return cannedNDJSON(ndjson), nil
		},
	})

	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.HandleChatCompletions(rec, req)

	if capturedTaste != true {
		t.Errorf("tasteLearning = %v, want true (binary default)", capturedTaste)
	}
}

// TestResponsesToChatRequest_PassesTasteLearning — the /v1/responses
// shim must propagate x_command_code_taste_learning to the chat
// request, otherwise per-request taste override is silently lost for
// Responses clients.
func TestResponsesToChatRequest_PassesTasteLearning(t *testing.T) {
	fa := false
	resp := api.OpenAIResponsesRequest{
		Model: "MiniMaxAI/MiniMax-M3",
		Input: "hello",
		XCommandCodeTasteLearning: &fa,
	}
	chat := responsesToChatRequest(resp)
	if chat.XCommandCodeTasteLearning == nil || *chat.XCommandCodeTasteLearning != false {
		t.Errorf("XCommandCodeTasteLearning = %v, want &false", chat.XCommandCodeTasteLearning)
	}

	tr := true
	resp.XCommandCodeTasteLearning = &tr
	chat = responsesToChatRequest(resp)
	if chat.XCommandCodeTasteLearning == nil || *chat.XCommandCodeTasteLearning != true {
		t.Errorf("XCommandCodeTasteLearning = %v, want &true", chat.XCommandCodeTasteLearning)
	}

	resp.XCommandCodeTasteLearning = nil
	chat = responsesToChatRequest(resp)
	if chat.XCommandCodeTasteLearning != nil {
		t.Errorf("XCommandCodeTasteLearning = %v, want nil", chat.XCommandCodeTasteLearning)
	}
}

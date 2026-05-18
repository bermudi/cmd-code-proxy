package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/dev2k6/command-code-proxy-server/internal/api"
	"github.com/dev2k6/command-code-proxy-server/internal/version"
)

const defaultBaseURL = "https://api.commandcode.ai"
const defaultTimeout = 300 * time.Second

// Proxy struct
type Proxy struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

// NewProxy creates a new proxy instance
func NewProxy(apiKey string) *Proxy {
	return &Proxy{
		APIKey:  apiKey,
		BaseURL: defaultBaseURL,
		Client:  &http.Client{Timeout: defaultTimeout},
	}
}

// BuildRequest builds the CommandCode request body
func (p *Proxy) BuildRequest(openAIReq api.OpenAIChatRequest) (api.CCRequestBody, error) {
	model := MapModel(openAIReq.Model)
	system, msgs := ExtractSystem(openAIReq.Messages)
	ccMessages := ConvertMessages(msgs)

	temperature := 0.3
	maxTokens := 64000
	if openAIReq.Temperature != nil {
		temperature = *openAIReq.Temperature
	}
	if openAIReq.MaxTokens != nil {
		maxTokens = *openAIReq.MaxTokens
	}

	ccBody := api.CCRequestBody{
		Config: api.CCConfig{
			WorkingDir:    ".",
			Date:          time.Now().Format("2006-01-02"),
			Environment:   "cli",
			Structure:     []string{},
			IsGitRepo:     false,
			CurrentBranch: "",
			MainBranch:    "main",
			GitStatus:     "",
			RecentCommits: []string{},
		},
		Memory:   "",
		Taste:    "",
		Skills:   "",
		Params: api.CCChatParams{
			Model:       model,
			Messages:    ccMessages,
			Tools:       []any{},
			System:      system,
			MaxTokens:   maxTokens,
			Temperature: temperature,
			Stream:      true,
		},
		ThreadID: uuid.New().String(),
	}

	return ccBody, nil
}

// CreateUpstreamRequest creates a new HTTP request to the CommandCode API
func (p *Proxy) CreateUpstreamRequest(ctx context.Context, ccBody api.CCRequestBody, apiKey string) (*http.Request, error) {
	reqJSON, err := json.Marshal(ccBody)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}

	ccReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.BaseURL+"/alpha/generate", bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}

	ccReq.Header.Set("Content-Type", "application/json")
	ccReq.Header.Set("Authorization", "Bearer "+apiKey)
	ccReq.Header.Set("x-command-code-version", version.GetCommandCodeVersion())
	ccReq.Header.Set("x-cli-environment", "production")
	ccReq.Header.Set("Accept", "text/event-stream")

	return ccReq, nil
}

// CallUpstream makes the request to CommandCode API
func (p *Proxy) CallUpstream(req *http.Request) (*http.Response, error) {
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream error: %w", err)
	}
	return resp, nil
}

// HandleChatCompletions handles the /v1/chat/completions endpoint
func (p *Proxy) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":{"message":"Method not allowed"}}`, http.StatusMethodNotAllowed)
		return
	}

	// Get API key from client Authorization header or server default
	apiKey := r.Header.Get("Authorization")
	if apiKey != "" {
		apiKey = strings.TrimPrefix(apiKey, "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
	} else if p.APIKey != "" {
		apiKey = p.APIKey
	} else {
		http.Error(w, `{"error":{"message":"API key required. Set Authorization header."}}`, http.StatusUnauthorized)
		return
	}

	// Read request
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"Failed to read body"}}`, http.StatusBadRequest)
		return
	}

	var openAIReq api.OpenAIChatRequest
	if err := json.Unmarshal(body, &openAIReq); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"Invalid JSON: %s"}}`, err.Error()), http.StatusBadRequest)
		return
	}

	if len(openAIReq.Messages) == 0 {
		http.Error(w, `{"error":{"message":"messages array is required"}}`, http.StatusBadRequest)
		return
	}

	// Build CommandCode request
	ccBody, err := p.BuildRequest(openAIReq)
	if err != nil {
		http.Error(w, `{"error":{"message":"Failed to build request"}}`, http.StatusInternalServerError)
		return
	}

	// Create upstream request
	ccReq, err := p.CreateUpstreamRequest(r.Context(), ccBody, apiKey)
	if err != nil {
		http.Error(w, `{"error":{"message":"Failed to create upstream request"}}`, http.StatusInternalServerError)
		return
	}

	// Call upstream
	ccResp, err := p.CallUpstream(ccReq)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer ccResp.Body.Close()

	if ccResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(ccResp.Body)
		log.Printf("[ERROR] Upstream returned %d: %s", ccResp.StatusCode, string(errBody))
		http.Error(w, fmt.Sprintf(`{"error":{"message":"Upstream error: %s"}}`, string(errBody)), http.StatusBadGateway)
		return
	}

	requestID := "chatcmpl-" + uuid.New().String()[:29]
	created := time.Now().Unix()

	if openAIReq.Stream {
		p.StreamResponse(w, r, ccResp, requestID, ccBody.Params.Model, created)
	} else {
		p.NonStreamResponse(w, ccResp, requestID, ccBody.Params.Model, created)
	}
}

// StreamResponse handles streaming response from CommandCode to OpenAI SSE
func (p *Proxy) StreamResponse(w http.ResponseWriter, r *http.Request, ccResp *http.Response, requestID, model string, created int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":{"message":"Streaming not supported"}}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	scanner := bufio.NewScanner(ccResp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	sentRole := false

	for scanner.Scan() {
		select {
		case <-r.Context().Done():
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event api.CCStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event.Type {
		case "text-delta":
			delta := api.OpenAIDelta{Content: event.Text}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case "finish":
			reason := "stop"
			if event.FinishReason == "tool_calls" {
				reason = "tool_calls"
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
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

		case "error":
			log.Printf("[ERROR] Stream error: %v", event.Error)
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		log.Printf("[ERROR] Scanner error: %v", err)
	}
}

// WriteSSE writes a Server-Sent Event
func (p *Proxy) WriteSSE(w io.Writer, flusher http.Flusher, resp api.OpenAIChatResponse) {
	data, _ := json.Marshal(resp)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// NonStreamResponse handles non-streaming response
func (p *Proxy) NonStreamResponse(w http.ResponseWriter, ccResp *http.Response, requestID, model string, created int64) {
	scanner := bufio.NewScanner(ccResp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var content strings.Builder
	var inputTokens, outputTokens int

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event api.CCStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event.Type {
		case "text-delta":
			content.WriteString(event.Text)
		case "finish":
			if event.TotalUsage != nil {
				inputTokens = event.TotalUsage.InputTokens
				outputTokens = event.TotalUsage.OutputTokens
			}
		case "error":
			log.Printf("[ERROR] Stream error: %v", event.Error)
		}
	}

	response := api.OpenAIChatResponse{
		ID:      requestID,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []api.OpenAIChoice{{
			Index: 0,
			Message: &api.OpenAIMessage{
				Role:    "assistant",
				Content: content.String(),
			},
			FinishReason: new(string),
		}},
		Usage: &api.OpenAIUsage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		},
	}
	*response.Choices[0].FinishReason = "stop"

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// HandleModels handles the /v1/models endpoint
func (p *Proxy) HandleModels(w http.ResponseWriter, r *http.Request) {
	models := api.OpenAIModelList{
		Object: "list",
		Data: []api.OpenAIModel{
			// MoonshotAI
			{ID: "moonshotai/Kimi-K2.6", Object: "model", Created: 0, OwnedBy: "moonshotai"},
			{ID: "moonshotai/Kimi-K2.5", Object: "model", Created: 0, OwnedBy: "moonshotai"},
			// ZhipuAI
			{ID: "zai-org/GLM-5.1", Object: "model", Created: 0, OwnedBy: "zhipuai"},
			{ID: "zai-org/GLM-5", Object: "model", Created: 0, OwnedBy: "zhipuai"},
			// MiniMaxAI
			{ID: "MiniMaxAI/MiniMax-M2.7", Object: "model", Created: 0, OwnedBy: "minimaxai"},
			{ID: "MiniMaxAI/MiniMax-M2.5", Object: "model", Created: 0, OwnedBy: "minimaxai"},
			// DeepSeek
			{ID: "deepseek/deepseek-v4-pro", Object: "model", Created: 0, OwnedBy: "deepseek"},
			{ID: "deepseek/deepseek-v4-flash", Object: "model", Created: 0, OwnedBy: "deepseek"},
			// Qwen
			{ID: "Qwen/Qwen3.6-Max-Preview", Object: "model", Created: 0, OwnedBy: "qwen"},
			{ID: "Qwen/Qwen3.6-Plus", Object: "model", Created: 0, OwnedBy: "qwen"},
			// StepFun
			{ID: "stepfun/Step-3.5-Flash", Object: "model", Created: 0, OwnedBy: "stepfun"},
			// Google
			{ID: "google/gemini-3.1-flash-lite", Object: "model", Created: 0, OwnedBy: "google"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

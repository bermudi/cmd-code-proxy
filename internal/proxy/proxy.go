package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
	"github.com/dev2k6/command-code-proxy-server/internal/version"
	"github.com/google/uuid"
)

const defaultBaseURL = "https://api.commandcode.ai"
const defaultTimeout = 300 * time.Second
const debugLogLimit = 20000

const (
	maxRetries     = 3
	baseRetryDelay = 1 * time.Second
	maxRetryDelay  = 30 * time.Second
)

const modelsCacheTTL = 5 * time.Minute

func truncateLog(s string) string {
	if len(s) <= debugLogLimit {
		return s
	}
	return s[:debugLogLimit] + fmt.Sprintf("... [truncated %d bytes]", len(s)-debugLogLimit)
}

func (p *Proxy) debugf(format string, args ...any) {
	if p.Debug {
		log.Printf(format, args...)
	}
}

func (p *Proxy) writeOpenAIError(w http.ResponseWriter, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.OpenAIErrorResponse{Error: api.OpenAIError{
		Message: message,
		Type:    errType,
		Param:   nil,
		Code:    nil,
	}})
}

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

// Proxy struct
type Proxy struct {
	APIKey           string
	BaseURL          string
	Client           *http.Client
	Debug            bool
	ListClosedModels bool
	modelsCache      []api.OpenAIModel
	modelsCacheTime  time.Time
	modelsCacheMu    sync.RWMutex
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
	if openAIReq.MaxCompletionTokens != nil {
		maxTokens = *openAIReq.MaxCompletionTokens
	}

	tools := ConvertTools(openAIReq.Tools)

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
		Memory: "",
		Taste:  "",
		Skills: "",
		Params: api.CCChatParams{
			Model:       model,
			Messages:    ccMessages,
			Tools:       tools,
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

	p.debugf("[DEBUG] CommandCode request body: %s", truncateLog(string(reqJSON)))

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

// DoUpstream calls upstream with retry on 429/5xx
func (p *Proxy) DoUpstream(ctx context.Context, ccBody api.CCRequestBody, apiKey string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := calculateBackoff(attempt)
			if delay > maxRetryDelay {
				delay = maxRetryDelay
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		ccReq, err := p.CreateUpstreamRequest(ctx, ccBody, apiKey)
		if err != nil {
			return nil, err
		}

		resp, err := p.Client.Do(ccReq)
		if err != nil {
			lastErr = err
			p.debugf("[DEBUG] Request failed (attempt %d/%d): %v", attempt+1, maxRetries+1, err)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode >= 500 && resp.StatusCode < 600) {
			lastErr = fmt.Errorf("upstream status %d", resp.StatusCode)
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if retryAfter > 0 {
				p.debugf("[DEBUG] Upstream %d, Retry-After=%s, retrying (%d/%d): %s", resp.StatusCode, retryAfter, attempt+1, maxRetries+1, string(body))
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(retryAfter):
				}
			} else {
				p.debugf("[DEBUG] Upstream %d, retrying (%d/%d): %s", resp.StatusCode, attempt+1, maxRetries+1, string(body))
			}
			continue
		}

		return resp, nil
	}
	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if sec, err := strconv.Atoi(value); err == nil && sec > 0 {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		until := time.Until(t)
		if until > 0 {
			return until
		}
	}
	return 0
}

func calculateBackoff(attempt int) time.Duration {
	base := baseRetryDelay * time.Duration(1<<attempt)    // 1s, 2s, 4s
	jitter := time.Duration(rand.Int63n(int64(base) / 5)) // ±20%
	return base + jitter
}

func extractOwner(modelID string) string {
	parts := strings.SplitN(modelID, "/", 2)
	if len(parts) >= 2 {
		return parts[0]
	}
	return "unknown"
}

func isOpenModel(m api.OpenAIModel) bool {
	// Open-source providers use provider/model format (e.g. "deepseek/deepseek-v4-pro").
	// Closed/premium models (Claude, GPT) use bare IDs without a slash.
	return strings.Contains(m.ID, "/")
}

func filterModels(models []api.OpenAIModel, includeClosed bool) []api.OpenAIModel {
	if includeClosed {
		return models
	}
	openModels := make([]api.OpenAIModel, 0, len(models))
	for _, m := range models {
		if isOpenModel(m) {
			openModels = append(openModels, m)
		}
	}
	return openModels
}

// FetchModels fetches model list from upstream with caching
func (p *Proxy) FetchModels(apiKey string) ([]api.OpenAIModel, error) {
	p.modelsCacheMu.RLock()
	if len(p.modelsCache) > 0 && time.Since(p.modelsCacheTime) < modelsCacheTTL {
		cached := p.modelsCache
		p.modelsCacheMu.RUnlock()
		return cached, nil
	}
	p.modelsCacheMu.RUnlock()

	req, err := http.NewRequest(http.MethodGet, p.BaseURL+"/provider/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	var modelList api.OpenAIModelList
	if err := json.NewDecoder(resp.Body).Decode(&modelList); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}

	models := make([]api.OpenAIModel, len(modelList.Data))
	for i, m := range modelList.Data {
		owner := m.OwnedBy
		if owner == "" {
			owner = extractOwner(m.ID)
		}
		models[i] = api.OpenAIModel{
			ID:      m.ID,
			Object:  "model",
			Created: 0,
			OwnedBy: owner,
		}
	}
	filtered := filterModels(models, p.ListClosedModels)

	p.modelsCacheMu.Lock()
	p.modelsCache = filtered
	p.modelsCacheTime = time.Now()
	p.modelsCacheMu.Unlock()

	return filtered, nil
}

// HandleChatCompletions handles the /v1/chat/completions endpoint
func (p *Proxy) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
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
		p.writeOpenAIError(w, http.StatusUnauthorized, "API key required. Set Authorization header.", "authentication_error")
		return
	}

	// Read request
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, "Failed to read body", "invalid_request_error")
		return
	}

	p.debugf("[DEBUG] Client request body: %s", truncateLog(string(body)))

	var openAIReq api.OpenAIChatRequest
	if err := json.Unmarshal(body, &openAIReq); err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %s", err.Error()), "invalid_request_error")
		return
	}

	if len(openAIReq.Messages) == 0 {
		p.writeOpenAIError(w, http.StatusBadRequest, "messages array is required", "invalid_request_error")
		return
	}

	// Build CommandCode request
	ccBody, err := p.BuildRequest(openAIReq)
	if err != nil {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to build request", "server_error")
		return
	}

	// Call upstream with retry
	ccResp, err := p.DoUpstream(r.Context(), ccBody, apiKey)
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadGateway, err.Error(), "api_error")
		return
	}
	defer ccResp.Body.Close()

	if ccResp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(ccResp.Body)
		message := fmt.Sprintf("Upstream error: %s", string(errBody))
		log.Printf("[ERROR] Upstream returned %d: %s", ccResp.StatusCode, string(errBody))
		// Log request body on 4xx for debugging
		if ccResp.StatusCode >= http.StatusBadRequest && ccResp.StatusCode < http.StatusInternalServerError {
			reqJSON, _ := json.Marshal(ccBody)
			log.Printf("[ERROR] Request body that caused %d: %s", ccResp.StatusCode, truncateLog(string(reqJSON)))
		}
		status := http.StatusBadGateway
		if ccResp.StatusCode >= http.StatusBadRequest && ccResp.StatusCode < http.StatusInternalServerError {
			status = ccResp.StatusCode
		}
		p.writeOpenAIError(w, status, message, "api_error")
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
		p.writeOpenAIError(w, http.StatusInternalServerError, "Streaming not supported", "server_error")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	scanner := bufio.NewScanner(ccResp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	sentRole := false
	toolCallIndex := 0
	toolCallIndexes := map[string]int{}

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
		p.debugf("[DEBUG] CommandCode stream line: %s", truncateLog(line))

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

		case "reasoning-start":
			// State change: entering reasoning mode. No output needed.

		case "reasoning-delta":
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

		case "reasoning-end":
			// State change: exiting reasoning mode. No output needed.

		case "tool-result":
			// Tool result events in stream are informational; no action needed.

		case "tool-use":
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
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})
			toolCallIndex++

		case "tool-delta":
			toolCalls := []api.OpenAIDeltaToolCall{{
				Index:    toolCallIndex - 1,
				Function: &api.OpenAIDeltaFunction{Arguments: event.Text},
			}}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &api.OpenAIDelta{ToolCalls: toolCalls}}},
			})

		case "tool-input-start":
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
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case "tool-input-delta":
			idx, ok := toolCallIndexes[event.ID]
			if !ok {
				idx = toolCallIndex
				toolCallIndexes[event.ID] = idx
				toolCallIndex++
			}
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &api.OpenAIDelta{ToolCalls: []api.OpenAIDeltaToolCall{{
					Index:    idx,
					Function: &api.OpenAIDeltaFunction{Arguments: event.Delta},
				}}}}},
			})

		case "tool-call":
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
			p.WriteSSE(w, flusher, api.OpenAIChatResponse{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []api.OpenAIChoice{{Index: 0, Delta: &delta}},
			})

		case "finish":
			reason := normalizeFinishReason(event.FinishReason)
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
	var inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int
	var hasToolCalls bool
	var toolCalls []api.ToolCall
	toolCallByID := map[string]int{}
	toolInputBuffers := map[string]*strings.Builder{}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		p.debugf("[DEBUG] CommandCode stream line: %s", truncateLog(line))

		var event api.CCStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event.Type {
		case "text-delta":
			content.WriteString(event.Text)
		case "reasoning-start":
			// no-op
		case "reasoning-delta":
			content.WriteString(event.Text)
		case "reasoning-end":
			// no-op
		case "tool-use":
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
		case "tool-delta":
			if len(toolCalls) > 0 {
				toolCalls[len(toolCalls)-1].Function.Arguments += event.Text
			}
		case "tool-input-start":
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
		case "tool-input-delta":
			if b := toolInputBuffers[event.ID]; b != nil {
				b.WriteString(event.Delta)
			}
			if idx, ok := toolCallByID[event.ID]; ok {
				toolCalls[idx].Function.Arguments += event.Delta
			}
		case "tool-call":
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
		case "tool-result":
			// Tool result events in stream are informational; no action needed
		case "finish":
			if event.TotalUsage != nil {
				inputTokens = event.TotalUsage.InputTokens
				outputTokens = event.TotalUsage.OutputTokens
				cacheReadTokens = event.TotalUsage.CacheReadInputTokens
				cacheWriteTokens = event.TotalUsage.CacheCreationInputTokens
			}
		case "error":
			log.Printf("[ERROR] Stream error: %v", event.Error)
		}
	}

	msg := &api.OpenAIMessage{
		Role:    "assistant",
		Content: content.String(),
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

func (p *Proxy) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, "Failed to read body", "invalid_request_error")
		return
	}

	p.debugf("[DEBUG] Client responses request body: %s", truncateLog(string(body)))

	var responsesReq api.OpenAIResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		p.writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %s", err.Error()), "invalid_request_error")
		return
	}

	chatReq := responsesToChatRequest(responsesReq)
	rewritten, err := json.Marshal(chatReq)
	if err != nil {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to build request", "server_error")
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))
	p.HandleChatCompletions(w, r)
}

func responsesToChatRequest(req api.OpenAIResponsesRequest) api.OpenAIChatRequest {
	messages := responsesInputToMessages(req.Input)
	if req.Instructions != nil {
		messages = append([]api.OpenAIMessage{{Role: "system", Content: req.Instructions}}, messages...)
	}

	maxTokens := req.MaxCompletionTokens
	if maxTokens == nil {
		maxTokens = req.MaxOutputTokens
	}
	if maxTokens == nil {
		maxTokens = req.MaxTokens
	}

	return api.OpenAIChatRequest{
		Model:               req.Model,
		Messages:            messages,
		Temperature:         req.Temperature,
		MaxTokens:           req.MaxTokens,
		MaxCompletionTokens: maxTokens,
		Stream:              req.Stream,
		Tools:               req.Tools,
		ToolChoice:          req.ToolChoice,
		ParallelToolCalls:   req.ParallelToolCalls,
		ResponseFormat:      req.ResponseFormat,
		Stop:                req.Stop,
		TopP:                req.TopP,
		User:                req.User,
	}
}

func responsesInputToMessages(input any) []api.OpenAIMessage {
	switch v := input.(type) {
	case nil:
		return nil
	case string:
		return []api.OpenAIMessage{{Role: "user", Content: v}}
	case []any:
		if messages := responseItemsToMessages(v); len(messages) > 0 {
			return messages
		}
		return []api.OpenAIMessage{{Role: "user", Content: v}}
	default:
		return []api.OpenAIMessage{{Role: "user", Content: v}}
	}
}

func responseItemsToMessages(items []any) []api.OpenAIMessage {
	messages := make([]api.OpenAIMessage, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		content := m["content"]
		if content == nil {
			content = m["text"]
		}
		if content == nil {
			content = m["input"]
		}
		messages = append(messages, api.OpenAIMessage{Role: role, Content: content})
	}
	return messages
}

// fallbackModels is used when dynamic fetch fails
var fallbackModels = []api.OpenAIModel{
	{ID: "moonshotai/Kimi-K2.6", Object: "model", Created: 0, OwnedBy: "moonshotai"},
	{ID: "moonshotai/Kimi-K2.5", Object: "model", Created: 0, OwnedBy: "moonshotai"},
	{ID: "zai-org/GLM-5.1", Object: "model", Created: 0, OwnedBy: "zhipuai"},
	{ID: "zai-org/GLM-5", Object: "model", Created: 0, OwnedBy: "zhipuai"},
	{ID: "MiniMaxAI/MiniMax-M2.7", Object: "model", Created: 0, OwnedBy: "minimaxai"},
	{ID: "MiniMaxAI/MiniMax-M2.5", Object: "model", Created: 0, OwnedBy: "minimaxai"},
	{ID: "MiniMaxAI/MiniMax-M3", Object: "model", Created: 0, OwnedBy: "minimaxai"},
	{ID: "deepseek/deepseek-v4-pro", Object: "model", Created: 0, OwnedBy: "deepseek"},
	{ID: "deepseek/deepseek-v4-flash", Object: "model", Created: 0, OwnedBy: "deepseek"},
	{ID: "Qwen/Qwen3.6-Max-Preview", Object: "model", Created: 0, OwnedBy: "qwen"},
	{ID: "Qwen/Qwen3.6-Plus", Object: "model", Created: 0, OwnedBy: "qwen"},
	{ID: "stepfun/Step-3.5-Flash", Object: "model", Created: 0, OwnedBy: "stepfun"},
	{ID: "stepfun/Step-3.7-Flash", Object: "model", Created: 0, OwnedBy: "stepfun"},
	{ID: "Qwen/Qwen3.7-Max-Free", Object: "model", Created: 0, OwnedBy: "qwen"},
	{ID: "Qwen/Qwen3.7-Max", Object: "model", Created: 0, OwnedBy: "qwen"},
	{ID: "xiaomi/mimo-v2.5-pro", Object: "model", Created: 0, OwnedBy: "xiaomi"},
	{ID: "xiaomi/mimo-v2.5", Object: "model", Created: 0, OwnedBy: "xiaomi"},
	{ID: "google/gemini-3.1-flash-lite", Object: "model", Created: 0, OwnedBy: "google"},
}

// HandleModels handles the /v1/models endpoint
func (p *Proxy) HandleModels(w http.ResponseWriter, r *http.Request) {
	apiKey := r.Header.Get("Authorization")
	if apiKey != "" {
		apiKey = strings.TrimPrefix(apiKey, "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
	} else if p.APIKey != "" {
		apiKey = p.APIKey
	}

	var models []api.OpenAIModel
	if apiKey != "" {
		var err error
		models, err = p.FetchModels(apiKey)
		if err != nil {
			p.debugf("[DEBUG] Failed to fetch models dynamically: %v. Using fallback.", err)
		}
	}
	if models == nil {
		models = filterModels(fallbackModels, p.ListClosedModels)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(api.OpenAIModelList{
		Object: "list",
		Data:   models,
	})
}

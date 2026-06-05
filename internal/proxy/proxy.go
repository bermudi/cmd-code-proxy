package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bermudi/cmd-code-proxy/internal/api"
	"github.com/google/uuid"
)

const debugLogLimit = 20000

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

func currentWorkingDir() string {
	workingDir, err := os.Getwd()
	if err != nil || workingDir == "" {
		return "."
	}
	return workingDir
}

func projectSlugFromPath(pathName string) string {
	var b strings.Builder
	lastWasDash := false

	pathName = strings.ToLower(pathName)
	if len(pathName) >= 2 && pathName[1] == ':' && pathName[0] >= 'a' && pathName[0] <= 'z' {
		pathName = pathName[2:]
	}

	for _, r := range pathName {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlnum {
			b.WriteRune(r)
			lastWasDash = false
			continue
		}
		if !lastWasDash && b.Len() > 0 {
			b.WriteByte('-')
			lastWasDash = true
		}
	}

	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "project"
	}
	return slug
}

// Proxy holds handler logic and an Upstream adapter.
type Proxy struct {
	APIKey           string
	Debug            bool
	ListClosedModels bool
	upstream         Upstream
}

// NewProxy creates a new proxy with the given upstream adapter.
func NewProxy(apiKey string, upstream Upstream) *Proxy {
	return &Proxy{
		APIKey:   apiKey,
		upstream: upstream,
	}
}

// BuildCCRequest builds the CommandCode request body (pure data transform).
func BuildCCRequest(openAIReq api.OpenAIChatRequest) (api.CCRequestBody, error) {
	model := MapModel(openAIReq.Model)
	system, msgs := ExtractSystem(openAIReq.Messages)
	ccMessages := ConvertMessages(msgs)
	workingDir := currentWorkingDir()

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
			WorkingDir:    workingDir,
			Date:          time.Now().Format("2006-01-02"),
			Environment:   "cli",
			Structure:     []string{},
			IsGitRepo:     false,
			CurrentBranch: "",
			MainBranch:    "",
			GitStatus:     "",
			RecentCommits: []string{},
		},
		Memory: nil,
		Taste:  nil,
		Skills: nil,
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
	ccBody, err := BuildCCRequest(openAIReq)
	if err != nil {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to build request", "server_error")
		return
	}

	// Call upstream
	respBody, err := p.upstream.Generate(r.Context(), ccBody, apiKey)
	if err != nil {
		var ue *UpstreamError
		if errors.As(err, &ue) {
			message := fmt.Sprintf("Upstream error: %s", ue.Body)
			log.Printf("[ERROR] Upstream returned %d: %s", ue.StatusCode, ue.Body)
			if ue.StatusCode >= http.StatusBadRequest && ue.StatusCode < http.StatusInternalServerError {
				reqJSON, _ := json.Marshal(ccBody)
				log.Printf("[ERROR] Request body that caused %d: %s", ue.StatusCode, truncateLog(string(reqJSON)))
			}
			status := http.StatusBadGateway
			if ue.StatusCode >= http.StatusBadRequest && ue.StatusCode < http.StatusInternalServerError {
				status = ue.StatusCode
			}
			p.writeOpenAIError(w, status, message, "api_error")
		} else {
			p.writeOpenAIError(w, http.StatusBadGateway, err.Error(), "api_error")
		}
		return
	}
	defer respBody.Close()

	requestID := "chatcmpl-" + uuid.New().String()[:29]
	created := time.Now().Unix()

	if openAIReq.Stream {
		p.StreamResponse(w, r, respBody, requestID, ccBody.Params.Model, created)
	} else {
		p.NonStreamResponse(w, respBody, requestID, ccBody.Params.Model, created)
	}
}

// StreamResponse handles streaming response from CommandCode to OpenAI SSE.
func (p *Proxy) StreamResponse(w http.ResponseWriter, r *http.Request, body io.ReadCloser, requestID, model string, created int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Streaming not supported", "server_error")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	t := NewEventTranslator(body)
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
		p.debugf("[DEBUG] CommandCode stream line: %s", truncateLog(event.RawLine))

		switch event.Type {
		case EventTextDelta:
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

		case EventReasoningStart:
			// State change: entering reasoning mode.

		case EventReasoningDelta:
			delta := api.OpenAIDelta{ReasoningContent: event.Text}
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

		case EventReasoningEnd:
			// State change: exiting reasoning mode.

		case EventToolResult:
			// Informational; no output needed.

		case EventToolUse:
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

		case EventToolDelta:
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

		case EventToolInputStart:
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

		case EventToolInputDelta:
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

		case EventToolCall:
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

		case EventFinish:
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

		case EventError:
			log.Printf("[ERROR] Stream error: %v", event.Error)
		}
	}

	if err := t.Err(); err != nil && err != io.EOF {
		log.Printf("[ERROR] Scanner error: %v", err)
	}
}

// WriteSSE writes a Server-Sent Event
func (p *Proxy) WriteSSE(w io.Writer, flusher http.Flusher, resp api.OpenAIChatResponse) {
	data, _ := json.Marshal(resp)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// NonStreamResponse handles non-streaming response by buffering the full stream.
func (p *Proxy) NonStreamResponse(w http.ResponseWriter, body io.ReadCloser, requestID, model string, created int64) {
	t := NewEventTranslator(body)

	var content strings.Builder
	var reasoningContent strings.Builder
	var inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int
	var hasToolCalls bool
	var toolCalls []api.ToolCall
	toolCallByID := map[string]int{}
	toolInputBuffers := map[string]*strings.Builder{}

	for t.Next() {
		event := t.Event()
		p.debugf("[DEBUG] CommandCode stream line: %s", truncateLog(event.RawLine))

		switch event.Type {
		case EventTextDelta:
			content.WriteString(event.Text)
		case EventReasoningStart:
			// no-op
		case EventReasoningDelta:
			reasoningContent.WriteString(event.Text)
		case EventReasoningEnd:
			// no-op
		case EventToolUse:
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
		case EventToolDelta:
			if len(toolCalls) > 0 {
				toolCalls[len(toolCalls)-1].Function.Arguments += event.Text
			}
		case EventToolInputStart:
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
		case EventToolInputDelta:
			if b := toolInputBuffers[event.ID]; b != nil {
				b.WriteString(event.Delta)
			}
			if idx, ok := toolCallByID[event.ID]; ok {
				toolCalls[idx].Function.Arguments += event.Delta
			}
		case EventToolCall:
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
		case EventToolResult:
			// Informational; no action needed.
		case EventFinish:
			if event.Usage != nil {
				inputTokens = event.Usage.InputTokens
				outputTokens = event.Usage.OutputTokens
				cacheReadTokens = event.Usage.CacheReadInputTokens
				cacheWriteTokens = event.Usage.CacheCreationInputTokens
			}
		case EventError:
			log.Printf("[ERROR] Stream error: %v", event.Error)
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
		models, err = p.upstream.FetchModels(r.Context(), apiKey)
		if err != nil {
			p.debugf("[DEBUG] Failed to fetch models dynamically: %v. Using fallback.", err)
		}
	}
	if len(models) == 0 {
		models = fallbackModels
	}
	models = filterModels(models, p.ListClosedModels)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(api.OpenAIModelList{
		Object: "list",
		Data:   models,
	})
}

func extractOwner(modelID string) string {
	parts := strings.SplitN(modelID, "/", 2)
	if len(parts) >= 2 {
		return parts[0]
	}
	return "unknown"
}

func isOpenModel(m api.OpenAIModel) bool {
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

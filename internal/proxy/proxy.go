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

// teeReadCloser wraps an io.ReadCloser, teeing every Read into a WriteCloser.
// When Close is called, both the source and the tee are closed.
type teeReadCloser struct {
	src io.ReadCloser
	tee io.WriteCloser
}

func newTeeReadCloser(src io.ReadCloser, tee io.WriteCloser) *teeReadCloser {
	return &teeReadCloser{src: src, tee: tee}
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		if _, werr := t.tee.Write(p[:n]); werr != nil {
			log.Printf("[WARN] capture: write error: %v", werr)
		}
	}
	return n, err
}

func (t *teeReadCloser) Close() error {
	srcErr := t.src.Close()
	teeErr := t.tee.Close()
	if srcErr != nil {
		return srcErr
	}
	return teeErr
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
	CaptureDir       string // if non-empty, tee upstream NDJSON to <CaptureDir>/<requestID>.ndjson
	upstream         Upstream
}

// NewProxy creates a new proxy with the given upstream adapter.
func NewProxy(apiKey string, upstream Upstream) *Proxy {
	return &Proxy{
		APIKey:   apiKey,
		upstream: upstream,
	}
}

// NewRouter builds the HTTP mux with all routes bound to the proxy's handlers.
func NewRouter(p *Proxy) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", logger(p.HandleChatCompletions))
	mux.HandleFunc("/chat/completions", logger(p.HandleChatCompletions))
	mux.HandleFunc("/v1/responses", logger(p.HandleResponses))
	mux.HandleFunc("/v1/models", logger(p.HandleModels))
	mux.HandleFunc("/health", logger(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	return mux
}

// logger is a middleware for logging requests.
func logger(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("[%s] %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		next(w, r)
		log.Printf("[%s] %s done in %v", r.Method, r.URL.Path, time.Since(start))
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

	// Tee upstream body to capture file if CaptureDir is set.
	if p.CaptureDir != "" {
		if f, err := os.CreateTemp(p.CaptureDir, requestID+"-*.ndjson"); err != nil {
			log.Printf("[WARN] capture: failed to create file in %s: %v", p.CaptureDir, err)
		} else {
			respBody = newTeeReadCloser(respBody, f)
		}
	}

	if openAIReq.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			p.writeOpenAIError(w, http.StatusInternalServerError, "Streaming not supported", "server_error")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		a := NewStreamAssembler(p, w, flusher, requestID, ccBody.Params.Model, created)
		_ = a.Run(r.Context(), respBody)
		return
	}
	_ = NewFinalAssembler(p, w, requestID, ccBody.Params.Model, created).Run(r.Context(), respBody)
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

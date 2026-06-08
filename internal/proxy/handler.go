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

	// Determine working directory: per-request field > flag > process cwd
	workingDir := openAIReq.XCommandCodeWorkingDir
	if workingDir == "" {
		workingDir = p.WorkingDir
	}

	// Build CommandCode request
	ccBody, err := BuildCCRequestWithWorkingDir(openAIReq, workingDir)
	if err != nil {
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to build request", "server_error")
		return
	}
	// BuildCCRequestWithWorkingDir drops system/developer messages before
	// converting. If every input message was system, we'd forward an
	// empty messages array to CommandCode and the gateway would 400 with
	// an opaque error. 400 here with a useful message instead.
	if len(ccBody.Params.Messages) == 0 {
		p.writeOpenAIError(w, http.StatusBadRequest,
			"no user/assistant/tool messages in request after dropping system content",
			"invalid_request_error")
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
		Model:                    req.Model,
		Messages:                 messages,
		Temperature:              req.Temperature,
		MaxTokens:                req.MaxTokens,
		MaxCompletionTokens:      maxTokens,
		Stream:                   req.Stream,
		Tools:                    req.Tools,
		ToolChoice:               req.ToolChoice,
		ParallelToolCalls:        req.ParallelToolCalls,
		ResponseFormat:           req.ResponseFormat,
		Stop:                     req.Stop,
		TopP:                     req.TopP,
		User:                     req.User,
		XCommandCodeWorkingDir:   req.XCommandCodeWorkingDir,
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

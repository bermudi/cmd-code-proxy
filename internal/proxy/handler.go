package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bermudi/cmd-code-proxy/internal/api"
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
	ctx context.Context
}

func newTeeReadCloser(ctx context.Context, src io.ReadCloser, tee io.WriteCloser) *teeReadCloser {
	return &teeReadCloser{src: src, tee: tee, ctx: ctx}
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		if _, werr := t.tee.Write(p[:n]); werr != nil {
			reqWarn(t.ctx, "capture write error", "error", werr)
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

	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Get API key from client Authorization header or server default
	apiKey := r.Header.Get("Authorization")
	if apiKey != "" {
		apiKey = strings.TrimPrefix(apiKey, "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
	} else if p.APIKey != "" {
		apiKey = p.APIKey
	} else {
		reqWarn(ctx, "unauthorized: no API key")
		p.writeOpenAIError(w, http.StatusUnauthorized, "API key required. Set Authorization header.", "authentication_error")
		return
	}

	// Read request
	body, err := io.ReadAll(r.Body)
	if err != nil {
		reqError(ctx, "failed to read request body", "error", err)
		p.writeOpenAIError(w, http.StatusBadRequest, "Failed to read body", "invalid_request_error")
		return
	}

	slog.Debug("client request body", "request_id", reqID, "body", truncateLog(string(body)))

	var openAIReq api.OpenAIChatRequest
	if err := json.Unmarshal(body, &openAIReq); err != nil {
		reqError(ctx, "invalid JSON", "error", err)
		p.writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %s", err.Error()), "invalid_request_error")
		return
	}

	if len(openAIReq.Messages) == 0 {
		reqWarn(ctx, "empty messages array")
		p.writeOpenAIError(w, http.StatusBadRequest, "messages array is required", "invalid_request_error")
		return
	}

	// Determine working directory: per-request field > flag > process cwd
	workingDir := openAIReq.XCommandCodeWorkingDir
	if workingDir == "" {
		workingDir = p.WorkingDir
	}

	// Resolve taste-learning preference: per-request field > proxy default > true.
	// See MAINTAINING.md § Taste learning for why the proxy doesn't just hardcode "true".
	tasteLearning := ResolveTasteLearning(openAIReq.XCommandCodeTasteLearning, p.TasteLearning)

	// Build CommandCode request
	ccBody, err := BuildCCRequestWithWorkingDir(openAIReq, workingDir)
	if err != nil {
		reqError(ctx, "failed to build upstream request", "error", err)
		p.writeOpenAIError(w, http.StatusInternalServerError, "Failed to build request", "server_error")
		return
	}
	// BuildCCRequestWithWorkingDir drops system/developer messages before
	// converting. If every input message was system, we'd forward an
	// empty messages array to CommandCode and the gateway would 400 with
	// an opaque error. 400 here with a useful message instead.
	if len(ccBody.Params.Messages) == 0 {
		reqWarn(ctx, "all messages were system/developer — nothing to forward")
		p.writeOpenAIError(w, http.StatusBadRequest,
			"no user/assistant/tool messages in request after dropping system content",
			"invalid_request_error")
		return
	}

	// Inject request attrs into context so all downstream logging carries them.
	ctx = WithRequestAttrs(ctx, requestAttrs{
		Model:      ccBody.Params.Model,
		Stream:     openAIReq.Stream,
		WorkingDir: workingDir,
	})

	reqInfo(ctx, "request start",
		"messages", len(openAIReq.Messages),
		"tools", len(openAIReq.Tools),
	)
	start := time.Now()

	// Capture the request body before sending it — even if the upstream
	// call fails, we want to know what was sent for debugging.
	if p.CaptureDir != "" {
		reqJSON, _ := json.MarshalIndent(ccBody, "", "  ")
		reqPath := filepath.Join(p.CaptureDir, reqID+".request.json")
		if err := os.WriteFile(reqPath, reqJSON, 0o644); err != nil {
			reqWarn(ctx, "capture request write failed", "error", err, "path", reqPath)
		}
	}

	// Call upstream
	respBody, err := p.upstream.Generate(ctx, ccBody, apiKey, tasteLearning)
	if err != nil {
		var ue *UpstreamError
		if errors.As(err, &ue) {
			reqError(ctx, "upstream error",
				"status", ue.StatusCode,
				"body", truncateLog(ue.Body),
			)
			status := http.StatusBadGateway
			if ue.StatusCode >= http.StatusBadRequest && ue.StatusCode < http.StatusInternalServerError {
				status = ue.StatusCode
			}
			p.writeOpenAIError(w, status, fmt.Sprintf("Upstream error: %s", ue.Body), "api_error")
		} else {
			reqError(ctx, "upstream call failed", "error", err)
			p.writeOpenAIError(w, http.StatusBadGateway, err.Error(), "api_error")
		}
		return
	}
	defer respBody.Close()

	// The request ID from the middleware becomes the OpenAI response ID
	// (chatcmpl-<uuid>). The middleware already sets X-Request-Id on
	// every response.
	created := time.Now().Unix()

	// Tee the upstream response to a capture file when CaptureDir is set.
	if p.CaptureDir != "" {
		if f, err := os.CreateTemp(p.CaptureDir, reqID+"-*.ndjson"); err != nil {
			reqWarn(ctx, "capture response file creation failed", "error", err)
		} else {
			respBody = newTeeReadCloser(ctx, respBody, f)
		}
	}

	if openAIReq.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			reqError(ctx, "streaming not supported by transport")
			p.writeOpenAIError(w, http.StatusInternalServerError, "Streaming not supported", "server_error")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		a := NewStreamAssembler(ctx, p, w, flusher, reqID, ccBody.Params.Model, created)
		_ = a.Run(ctx, respBody)
	} else {
		_ = NewFinalAssembler(ctx, p, w, reqID, ccBody.Params.Model, created).Run(ctx, respBody)
	}

	reqInfo(ctx, "request done", "duration", time.Since(start).Round(time.Millisecond))
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

	slog.Debug("client responses request body", "request_id", RequestIDFromContext(r.Context()), "body", truncateLog(string(body)))

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
		XCommandCodeConfig:       req.XCommandCodeConfig,
		XCommandCodeMemory:       req.XCommandCodeMemory,
		XCommandCodeSkills:       req.XCommandCodeSkills,
		XCommandCodeTaste:        req.XCommandCodeTaste,
		XCommandCodeTasteLearning: req.XCommandCodeTasteLearning,
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
	ctx := r.Context()
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
		models, err = p.upstream.FetchModels(ctx, apiKey)
		if err != nil {
			slog.Debug("failed to fetch models dynamically, using fallback", "error", err)
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

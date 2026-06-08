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
	"os/exec"
	"path/filepath"
	"sort"
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

// resolveConfig picks the config source: if the pi extension sent a
// fully-populated config, use it directly (the extension runs in the
// project directory and has the real data). Otherwise fall back to
// populateConfigFromFS as a local-deployment stopgap.
func resolveConfig(clientConfig *api.CCConfig, workingDir string) api.CCConfig {
	if clientConfig != nil && clientConfig.WorkingDir != "" {
		return *clientConfig
	}
	return populateConfigFromFS(workingDir)
}

// populateConfigFromFS fills the gateway's config struct with real values
// from the live filesystem. The real command-code binary does this; the
// proxy previously sent hardcoded stubs (empty structure, isGitRepo=false,
// fake "Go proxy" environment), which made the server-side system prompt
// look like a generic environment announcement and tripped MiniMax-M3's
// prior for treating short input as system state.
//
// workingDir MUST be the project's working directory (from the cc-cwd
// extension or the -working-dir flag), NOT os.Getwd() — the proxy
// process runs from its own checkout dir. Reading the proxy's cwd would
// leak the proxy's own tree (go.mod, internal/, etc.) into the gateway's
// system prompt.
func populateConfigFromFS(workingDir string) api.CCConfig {
	cfg := api.CCConfig{
		WorkingDir:  workingDir,
		Date:        time.Now().Format("2006-01-02"),
		Environment: "linux-x64, Node.js v26.2.0", // matches command-code CLI v0.32.2
		Structure:   readDirNames(workingDir),
	}
	if isGitRepo(workingDir) {
		cfg.IsGitRepo = true
		cfg.CurrentBranch = gitOutput(workingDir, "branch", "--show-current")
		cfg.MainBranch = gitMainBranch(workingDir)
		cfg.GitStatus = gitStatusSummary(workingDir)
		cfg.RecentCommits = gitLogOneline(workingDir, 3) // real binary uses -3
	}
	return cfg
}

// dirBlocklist mirrors the real binary's getRootDirectoryStructure filter.
var dirBlocklist = map[string]bool{
	"node_modules": true, "dist": true, "build": true,
	".git": true, ".svn": true, ".hg": true,
	"coverage": true, ".nyc_output": true, ".cache": true,
	"tmp": true, "temp": true,
	".next": true, ".nuxt": true, "out": true,
}

func readDirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") || dirBlocklist[e.Name()] {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// gitMainBranch mirrors the real binary's logic: parse `git branch -r`
// output, checking for origin/main or origin/master, falling back to "main".
func gitMainBranch(dir string) string {
	out := gitOutput(dir, "branch", "-r")
	if out == "" {
		return "main"
	}
	if strings.Contains(out, "origin/main") {
		return "main"
	}
	if strings.Contains(out, "origin/master") {
		return "master"
	}
	return "main"
}

// gitStatusSummary mirrors the real binary's getGitStatus: summarize
// porcelain output as "M N, A N, D N, ?? N" or "Working tree clean".
func gitStatusSummary(dir string) string {
	out := gitOutput(dir, "status", "--porcelain")
	return summarizePorcelain(out)
}

// summarizePorcelain is the pure parsing logic, extracted for testing.
func summarizePorcelain(porcelain string) string {
	if porcelain == "" {
		return "Working tree clean"
	}
	lines := strings.Split(porcelain, "\n")
	var modified, added, deleted, untracked int
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, " M"):
			modified++
		case strings.HasPrefix(line, "A " ):
			added++
		case strings.HasPrefix(line, " D"):
			deleted++
		case strings.HasPrefix(line, "??"):
			untracked++
		}
	}
	var parts []string
	if modified > 0 {
		parts = append(parts, fmt.Sprintf("M %d", modified))
	}
	if added > 0 {
		parts = append(parts, fmt.Sprintf("A %d", added))
	}
	if deleted > 0 {
		parts = append(parts, fmt.Sprintf("D %d", deleted))
	}
	if untracked > 0 {
		parts = append(parts, fmt.Sprintf("?? %d", untracked))
	}
	summary := strings.Join(parts, ", ")
	if summary == "" {
		return porcelain // fallback to raw output
	}
	return summary
}

func gitLogOneline(dir string, n int) []string {
	out := gitOutput(dir, "log", "--oneline", "-n", fmt.Sprintf("%d", n))
	if out == "" {
		return []string{}
	}
	return strings.Split(out, "\n")
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
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
	WorkingDir       string // if non-empty, overrides the process working directory sent to CommandCode
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
	return BuildCCRequestWithWorkingDir(openAIReq, "")
}

// BuildCCRequestWithWorkingDir builds the CommandCode request body and allows
// callers to override the CLI-compatible working directory sent upstream.
func BuildCCRequestWithWorkingDir(openAIReq api.OpenAIChatRequest, workingDirOverride string) (api.CCRequestBody, error) {
	model := MapModel(openAIReq.Model)
	// Drop system/developer messages. CommandCode's gateway builds the
	// system prompt server-side from config.workingDir (it reads the
	// project's AGENTS.md, skills, etc. from disk). Forwarding the OpenAI
	// system message as a user turn causes the model to treat it as an
	// environment announcement and hallucinate an acknowledgement.
	ccMessages := ConvertMessages(DropSystemMessages(openAIReq.Messages))
	workingDir := currentWorkingDir()
	if workingDirOverride != "" {
		workingDir = workingDirOverride
	}

	maxTokens := 64000
	if openAIReq.MaxTokens != nil {
		maxTokens = *openAIReq.MaxTokens
	}
	if openAIReq.MaxCompletionTokens != nil {
		maxTokens = *openAIReq.MaxCompletionTokens
	}

	tools := ConvertTools(openAIReq.Tools)

	ccBody := api.CCRequestBody{
		Config:         resolveConfig(openAIReq.XCommandCodeConfig, workingDir),
		Memory:         nil,
		Taste:          nil,
		Skills:         "",
		PermissionMode: "auto-accept",
		Params: api.CCChatParams{
			Model:     model,
			Messages:  ccMessages,
			Tools:     tools,
			MaxTokens: maxTokens,
			Stream:    true,
		},
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

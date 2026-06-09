package proxy

import (
	"fmt"
	"os"

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

func currentWorkingDir() string {
	workingDir, err := os.Getwd()
	if err != nil || workingDir == "" {
		return "."
	}
	return workingDir
}

// Proxy holds handler logic and an Upstream adapter.
type Proxy struct {
	APIKey           string
	ListClosedModels bool
	CaptureDir       string // if non-empty, tee upstream NDJSON to <CaptureDir>/<requestID>.ndjson
	WorkingDir       string // if non-empty, overrides the process working directory sent to CommandCode
	// TasteLearning is the proxy-wide default for the upstream x-taste-learning
	// header. nil = use binary default (true). The per-request
	// x_command_code_taste_learning field from the client (typically the pi
	// extension, which reads userConfig.tasteLearning) wins when present.
	TasteLearning *bool
	upstream         Upstream
}

// ResolveTasteLearning picks the effective value with the precedence:
// per-request override (from x_command_code_taste_learning) > proxy default
// (from -taste-learning) > binary default (true).
func ResolveTasteLearning(perRequest *bool, proxyDefault *bool) bool {
	if perRequest != nil {
		return *perRequest
	}
	if proxyDefault != nil {
		return *proxyDefault
	}
	return true
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
	// environment announcement and respond accordingly — it's correctly
	// interpreting malformed input, not hallucinating.
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
		ThreadID: newThreadID(),
	}

	// Forward memory/skills/taste from the pi extension if provided.
	// The gateway builds these from disk if absent, but the extension
	// has the real files and can send them directly.
	if openAIReq.XCommandCodeMemory != "" {
		ccBody.Memory = openAIReq.XCommandCodeMemory
	}
	if openAIReq.XCommandCodeSkills != "" {
		ccBody.Skills = openAIReq.XCommandCodeSkills
	}
	if openAIReq.XCommandCodeTaste != "" {
		ccBody.Taste = openAIReq.XCommandCodeTaste
	}
	
	  // PATCH: Force empty array on final CC payload instead of null
    for i := range ccBody.Params.Messages {
        if ccBody.Params.Messages[i].Content == nil {
            ccBody.Params.Messages[i].Content = []api.CCContentPart{}
        }
    }

	return ccBody, nil
}

// newThreadID generates a UUID for CommandCode session continuity.
// The proxy generates one per request because the OpenAI API does not
// expose a session identifier that persists across turns.
func newThreadID() *string {
	s := uuid.New().String()
	return &s
}

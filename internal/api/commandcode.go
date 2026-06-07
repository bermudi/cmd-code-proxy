package api

// CommandCode API types (internal)

type CCToolOutput struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type CCContentPart struct {
	Type       string        `json:"type"`
	Text       *string       `json:"text,omitempty"`
	ID         *string       `json:"id,omitempty"`
	Name       *string       `json:"name,omitempty"`
	Input      any           `json:"input,omitempty"`
	ToolCallID *string       `json:"toolCallId,omitempty"`
	ToolName   *string       `json:"toolName,omitempty"`
	Output     *CCToolOutput `json:"output,omitempty"`
	ToolUseID  *string       `json:"tool_use_id,omitempty"`
	Content    any           `json:"content,omitempty"`
}

type CCMessage struct {
	Role    string          `json:"role"`
	Content []CCContentPart `json:"content"`
}

type CCChatParams struct {
	Model     string      `json:"model"`
	Messages  []CCMessage `json:"messages"`
	Tools     []any       `json:"tools"`
	MaxTokens int         `json:"max_tokens"`
	Stream    bool        `json:"stream"`
}

type CCConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"`
	Environment   string   `json:"environment"`
	Structure     []string `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []string `json:"recentCommits"`
}

type CCRequestBody struct {
	Config         CCConfig     `json:"config"`
	Memory         any          `json:"memory"`
	Taste          any          `json:"taste"`
	Skills         string       `json:"skills"`
	Params         CCChatParams `json:"params"`
	PermissionMode string       `json:"permissionMode,omitempty"`
	ThreadID       *string      `json:"threadId,omitempty"`
}

type CCStreamEvent struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	ID           string         `json:"id"`
	Delta        string         `json:"delta"`
	Input        map[string]any `json:"input"`
	ToolCallID   string         `json:"toolCallId"`
	ToolName     string         `json:"toolName"`
	FinishReason string         `json:"finishReason"`
	Error        *struct {
		Message    string `json:"message"`
		StatusCode *int   `json:"statusCode"`
	} `json:"error"`
	TotalUsage *struct {
		InputTokens              int `json:"inputTokens"`
		OutputTokens             int `json:"outputTokens"`
		CacheReadInputTokens     int `json:"cachedInputTokens,omitempty"`
		CacheCreationInputTokens int `json:"cacheCreationInputTokens,omitempty"`
	} `json:"totalUsage"`

	// Usage is the per-step usage object from finish-step events.
	// Structurally similar to TotalUsage but may carry richer details.
	Usage *struct {
		InputTokens              int `json:"inputTokens"`
		OutputTokens             int `json:"outputTokens"`
		CacheReadInputTokens     int `json:"cachedInputTokens,omitempty"`
		CacheCreationInputTokens int `json:"cacheCreationInputTokens,omitempty"`
	} `json:"usage"`
}

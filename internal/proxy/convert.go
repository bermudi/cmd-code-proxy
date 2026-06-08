package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bermudi/cmd-code-proxy/internal/api"
)

// DropSystemMessages returns a copy of msgs with every system or developer
// message removed.
//
// CommandCode's /alpha/generate expects params.messages to contain only
// user/assistant/tool turns. The real command-code CLI binary never sends
// system content in the request body — CommandCode's gateway builds the
// system prompt server-side from config.workingDir (it reads AGENTS.md and
// related project context from the project itself). Clients that forward
// system messages from upstream (pi's harness bakes the project AGENTS.md
// into the OpenAI system message) must drop them, otherwise the model sees
// a fake "user" turn that looks like an environment announcement and
// responds accordingly — it's correctly interpreting malformed input, not
// hallucinating. See convert_test.go and paritytest/ for the coverage.
func DropSystemMessages(msgs []api.OpenAIMessage) []api.OpenAIMessage {
	out := make([]api.OpenAIMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Convert OpenAI messages to CommandCode format
func ConvertMessages(openAIMsgs []api.OpenAIMessage) []api.CCMessage {
	var ccMsgs []api.CCMessage
	toolNames := map[string]string{}

	for _, m := range openAIMsgs {
		role := normalizeRole(m.Role)
		if role == "" {
			// normalizeRole returned "" — a non-CC-valid role slipped
			// past DropSystemMessages. Don't forward it; CommandCode's
			// gateway would 400 anyway, and silently rewriting to "user"
			// is the original AGENTS.md-leak bug.
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				toolNames[tc.ID] = tc.Function.Name
			}
		}

		if m.Role == "tool" {
			toolName := m.Name
			if toolName == "" {
				toolName = toolNames[m.ToolCallID]
			}
			if toolName == "" {
				toolName = "unknown"
			}
			contentStr := contentToString(m.Content)
			outputType := "text"
			if strings.HasPrefix(contentStr, "Error:") {
				outputType = "error-text"
			}
			// toolCallId is required for tool-result messages.
			if m.ToolCallID == "" {
				// Skip tool messages without a toolCallId — they can't be matched.
				continue
			}
			ccMsgs = append(ccMsgs, api.CCMessage{
				Role: "tool",
				Content: []api.CCContentPart{{
					Type:       "tool-result",
					ToolCallID: strPtr(m.ToolCallID),
					ToolName:   strPtr(toolName),
					Output: &api.CCToolOutput{
						Type:  outputType,
						Value: contentStr,
					},
				}},
			})
			continue
		}

		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			contentParts := parseContent(m.Content, toolNames)
			addedTools := map[string]bool{}
			for _, part := range contentParts {
				if part.Type == "tool-call" && part.ToolCallID != nil {
					addedTools[*part.ToolCallID] = true
				}
			}
			for _, tc := range m.ToolCalls {
				if addedTools[tc.ID] {
					continue
				}
				if tc.ID == "" || tc.Function.Name == "" {
					continue
				}
				contentParts = append(contentParts, api.CCContentPart{
					Type:       "tool-call",
					ToolCallID: strPtr(tc.ID),
					ToolName:   strPtr(tc.Function.Name),
					Input:      parseToolInput(tc.Function.Arguments),
				})
				addedTools[tc.ID] = true
			}
			ccMsgs = append(ccMsgs, api.CCMessage{Role: role, Content: contentParts})
			continue
		}

		ccMsgs = append(ccMsgs, api.CCMessage{Role: role, Content: parseContent(m.Content, toolNames)})
	}
	return ccMsgs
}

func ConvertTools(openAITools []any) []any {
	if len(openAITools) == 0 {
		return []any{}
	}

	tools := make([]any, 0, len(openAITools))
	for _, tool := range openAITools {
		toolMap, ok := tool.(map[string]any)
		if !ok {
			continue
		}

		toolType, _ := toolMap["type"].(string)
		if toolType != "function" {
			tools = append(tools, toolMap)
			continue
		}

		fn, ok := toolMap["function"].(map[string]any)
		if !ok {
			continue
		}

		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}

		inputSchema, ok := fn["parameters"].(map[string]any)
		if !ok || inputSchema == nil {
			inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
		}

		ccTool := map[string]any{"name": name, "input_schema": inputSchema}
		if description, ok := fn["description"].(string); ok && description != "" {
			ccTool["description"] = description
		}
		tools = append(tools, ccTool)
	}

	return tools
}

func parseToolInput(arguments string) any {
	if arguments == "" {
		return map[string]any{}
	}
	var input any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return map[string]any{"arguments": arguments}
	}
	return input
}

// parseToolInputJSON handles raw JSON arguments that may already be parsed (map/string/any).
func parseToolInputJSON(input any) any {
	if input == nil {
		return map[string]any{}
	}
	if _, ok := input.(map[string]any); ok {
		return input
	}
	if s, ok := input.(string); ok && s != "" {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			return parsed
		}
		return map[string]any{"arguments": s}
	}
	return input
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func contentToString(content interface{}) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if partMap, ok := part.(map[string]any); ok {
				text := contentPartToString(partMap)
				if text != "" {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(data)
	}
}

func contentPartToString(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				b.WriteString(contentPartToString(m))
			}
		}
		return b.String()
	case map[string]any:
		for _, key := range []string{"text", "content", "output_text", "input_text", "refusal", "thinking", "redacted_thinking", "reasoning"} {
			if text, ok := v[key].(string); ok {
				return text
			}
		}
		if imgURL, ok := v["image_url"].(map[string]any); ok {
			if url, ok := imgURL["url"].(string); ok {
				return "[Image URL: " + url + "]"
			}
		}
		if url, ok := v["image_url"].(string); ok {
			return "[Image URL: " + url + "]"
		}
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(data)
	default:
		return fmt.Sprint(v)
	}
}

func parseContent(content interface{}, toolNames map[string]string) []api.CCContentPart {
	switch v := content.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		return []api.CCContentPart{{Type: "text", Text: strPtr(v)}}
	case []any:
		var parts []api.CCContentPart
		for _, part := range v {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := partMap["type"].(string)
			switch typ {
			case "text", "input_text", "output_text", "refusal", "document", "search_result":
				if text := contentPartToString(partMap); text != "" {
					parts = append(parts, api.CCContentPart{Type: "text", Text: strPtr(text)})
				}
			case "thinking", "redacted_thinking", "reasoning":
				if text := contentPartToString(partMap); text != "" {
					parts = append(parts, api.CCContentPart{Type: "reasoning", Text: strPtr(text)})
				}
			case "image_url", "input_image", "image":
				if text := contentPartToString(partMap); text != "" {
					parts = append(parts, api.CCContentPart{Type: "text", Text: strPtr(text)})
				}
			case "tool_use", "tool-call", "tool_call", "toolCall":
				id, _ := partMap["id"].(string)
				if id == "" {
					id, _ = partMap["toolCallId"].(string)
				}
				if id == "" {
					id, _ = partMap["tool_use_id"].(string)
				}
				if id == "" {
					id, _ = partMap["tool_call_id"].(string)
				}
				name, _ := partMap["name"].(string)
				if name == "" {
					name, _ = partMap["toolName"].(string)
				}
				if name == "" {
					if fn, ok := partMap["function"].(map[string]any); ok {
						name, _ = fn["name"].(string)
					}
				}
				if id != "" && name != "" && toolNames != nil {
					toolNames[id] = name
				}
				// Only emit tool-call content part if both id and name are present.
				// Missing fields cause upstream validation errors (400).
				if id == "" || name == "" {
					continue
				}
				input := partMap["input"]
				if input == nil {
					input = partMap["arguments"]
				}
				if input == nil {
					if fn, ok := partMap["function"].(map[string]any); ok {
						input = fn["arguments"]
					}
				}
				parts = append(parts, api.CCContentPart{
					Type:       "tool-call",
					ToolCallID: strPtr(id),
					ToolName:   strPtr(name),
					Input:      parseToolInputJSON(input),
				})
			case "tool_result", "tool-result", "toolResult":
				toolID, _ := partMap["tool_use_id"].(string)
				if toolID == "" {
					toolID, _ = partMap["toolCallId"].(string)
				}
				if toolID == "" {
					toolID, _ = partMap["id"].(string)
				}
				if toolID == "" {
					toolID, _ = partMap["tool_call_id"].(string)
				}
				toolName, _ := partMap["toolName"].(string)
				if toolName == "" {
					toolName = toolNames[toolID]
				}
				if toolName == "" {
					toolName = "unknown"
				}
				contentVal := contentPartToString(partMap["content"])
				if contentVal == "" {
					contentVal = contentPartToString(partMap["output"])
				}
				outputType := "text"
				if strings.HasPrefix(contentVal, "Error:") {
					outputType = "error-text"
				}
				// toolCallId is required for tool-result; skip if missing.
				if toolID == "" {
					continue
				}
				parts = append(parts, api.CCContentPart{
					Type:       "tool-result",
					ToolCallID: strPtr(toolID),
					ToolName:   strPtr(toolName),
					Output: &api.CCToolOutput{
						Type:  outputType,
						Value: contentVal,
					},
				})
			}
		}
		return parts
	default:
		return []api.CCContentPart{{Type: "text", Text: strPtr(contentToString(v))}}
	}
}

// normalizeRole maps OpenAI role names to CommandCode-valid roles.
// CC accepts: "user" | "assistant" | "tool".
//
// System and developer messages must be dropped by DropSystemMessages
// before this is called. If a non-CC-valid role reaches here, that's
// a programmer error in the proxy itself — the only safe responses are
// to panic (loud) or to emit an empty string and let the upstream
// gateway 400 (also loud, but at the right layer). We choose the empty
// string. ConvertMessages must never be called with a slice that still
// contains system/developer turns.
func normalizeRole(role string) string {
	switch role {
	case "user", "assistant", "tool":
		return role
	default:
		return ""
	}
}

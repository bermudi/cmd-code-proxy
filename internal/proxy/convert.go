package proxy

import (
	"strings"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

// Convert OpenAI messages to CommandCode format
func ConvertMessages(openAIMsgs []api.OpenAIMessage) []api.CCMessage {
	var ccMsgs []api.CCMessage
	for _, m := range openAIMsgs {
		ccMsgs = append(ccMsgs, api.CCMessage{
			Role: m.Role,
			Content: []api.CCContentPart{
				{Type: "text", Text: m.Content},
			},
		})
	}
	return ccMsgs
}

// Extract system message and remaining messages
func ExtractSystem(msgs []api.OpenAIMessage) (string, []api.OpenAIMessage) {
	var system strings.Builder
	var rest []api.OpenAIMessage
	for _, m := range msgs {
		if m.Role == "system" {
			if system.Len() > 0 {
				system.WriteString("\n")
			}
			system.WriteString(m.Content)
		} else {
			rest = append(rest, m)
		}
	}
	return system.String(), rest
}

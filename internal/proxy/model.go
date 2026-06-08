package proxy

import (
	"strings"

	"github.com/bermudi/cmd-code-proxy/internal/api"
)

// Map model name if client sends short name
func MapModel(name string) string {
	switch strings.ToLower(name) {
	case "deepseek-v4-pro", "deepseek-v4", "deepseek-pro":
		return "deepseek/deepseek-v4-pro"
	case "deepseek-v4-flash", "deepseek-flash":
		return "deepseek/deepseek-v4-flash"
	case "minimax-m2.7", "minimax2.7":
		return "MiniMaxAI/MiniMax-M2.7"
	case "minimax-m2.5", "minimax2.5", "minimax":
		return "MiniMaxAI/MiniMax-M2.5"
	case "MiniMaxAI/MiniMax-M3", "minimax-m3", "minimax3":
		return "MiniMaxAI/MiniMax-M3"
	case "glm-5.1":
		return "zai-org/GLM-5.1"
	case "glm-5":
		return "zai-org/GLM-5"
	case "kimi-k2.6", "kimi2.6":
		return "moonshotai/Kimi-K2.6"
	case "kimi-k2.5", "kimi2.5":
		return "moonshotai/Kimi-K2.5"
	case "qwen-3.6-max-preview", "qwen3.6-max":
		return "Qwen/Qwen3.6-Max-Preview"
	case "qwen-3.6-plus", "qwen3.6-plus", "qwen3.6":
		return "Qwen/Qwen3.6-Plus"
	case "step-3.5-flash", "step3.5":
		return "stepfun/Step-3.5-Flash"
	case "step-3.7-flash", "step3.7", "stepfun/Step-3.7-Flash":
		return "stepfun/Step-3.7-Flash"
	case "gemini-3.1-flash-lite", "gemini-flash-lite":
		return "google/gemini-3.1-flash-lite"
	case "qwen-3.7-max-free", "qwen3.7-max-free", "Qwen/Qwen3.7-Max-Free":
		return "Qwen/Qwen3.7-Max-Free"
	case "qwen-3.7-max", "qwen3.7-max", "Qwen/Qwen3.7-Max":
		return "Qwen/Qwen3.7-Max"
	case "mimo-v2.5-pro", "mimo-pro", "xiaomi/mimo-v2.5-pro":
		return "xiaomi/mimo-v2.5-pro"
	case "mimo-v2.5", "mimo", "xiaomi/mimo-v2.5":
		return "xiaomi/mimo-v2.5"
	default:
		return name // pass through as-is
	}
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

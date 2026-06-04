package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

func TestMapModel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Existing short aliases
		{"deepseek-v4-pro", "deepseek/deepseek-v4-pro"},
		{"deepseek-v4", "deepseek/deepseek-v4-pro"},
		{"DEEPSEEK-PRO", "deepseek/deepseek-v4-pro"},
		{"deepseek-v4-flash", "deepseek/deepseek-v4-flash"},
		{"deepseek-flash", "deepseek/deepseek-v4-flash"},
		{"minimax-m2.7", "MiniMaxAI/MiniMax-M2.7"},
		{"minimax-m2.5", "MiniMaxAI/MiniMax-M2.5"},
		{"minimax", "MiniMaxAI/MiniMax-M2.5"},
		{"glm-5.1", "zai-org/GLM-5.1"},
		{"glm-5", "zai-org/GLM-5"},
		{"kimi-k2.6", "moonshotai/Kimi-K2.6"},
		{"kimi-k2.5", "moonshotai/Kimi-K2.5"},
		{"qwen-3.6-max-preview", "Qwen/Qwen3.6-Max-Preview"},
		{"qwen3.6", "Qwen/Qwen3.6-Plus"},
		{"step-3.5-flash", "stepfun/Step-3.5-Flash"},
		{"gemini-3.1-flash-lite", "google/gemini-3.1-flash-lite"},

		// New short aliases
		{"minimax-m3", "MiniMaxAI/MiniMax-M3"},
		{"minimax3", "MiniMaxAI/MiniMax-M3"},
		{"qwen-3.7-max-free", "Qwen/Qwen3.7-Max-Free"},
		{"qwen3.7-max-free", "Qwen/Qwen3.7-Max-Free"},
		{"qwen-3.7-max", "Qwen/Qwen3.7-Max"},
		{"qwen3.7-max", "Qwen/Qwen3.7-Max"},
		{"step-3.7-flash", "stepfun/Step-3.7-Flash"},
		{"step3.7", "stepfun/Step-3.7-Flash"},
		{"mimo-v2.5-pro", "xiaomi/mimo-v2.5-pro"},
		{"mimo-pro", "xiaomi/mimo-v2.5-pro"},
		{"mimo-v2.5", "xiaomi/mimo-v2.5"},
		{"mimo", "xiaomi/mimo-v2.5"},

		// New full IDs pass through
		{"MiniMaxAI/MiniMax-M3", "MiniMaxAI/MiniMax-M3"},
		{"Qwen/Qwen3.7-Max-Free", "Qwen/Qwen3.7-Max-Free"},
		{"Qwen/Qwen3.7-Max", "Qwen/Qwen3.7-Max"},
		{"stepfun/Step-3.7-Flash", "stepfun/Step-3.7-Flash"},
		{"xiaomi/mimo-v2.5-pro", "xiaomi/mimo-v2.5-pro"},
		{"xiaomi/mimo-v2.5", "xiaomi/mimo-v2.5"},

		// Unknown names pass through unchanged
		{"some/unknown-model", "some/unknown-model"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"", ""},
	}
	for _, c := range cases {
		got := MapModel(c.in)
		if got != c.want {
			t.Errorf("MapModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Second},
		{"60", 60 * time.Second},
		{"invalid", 0},
		{"-1", 0},
	}
	for _, c := range cases {
		got := parseRetryAfter(c.input)
		if got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	// Parse a future HTTP date
	future := time.Now().UTC().Add(10 * time.Second)
	got := parseRetryAfter(future.Format(http.TimeFormat))
	if got <= 0 || got > 11*time.Second {
		t.Errorf("parseRetryAfter(http-date) = %v, expected ~10s", got)
	}
	// Parse a past HTTP date
	past := time.Now().UTC().Add(-10 * time.Second)
	got2 := parseRetryAfter(past.Format(http.TimeFormat))
	if got2 != 0 {
		t.Errorf("parseRetryAfter(past http-date) = %v, want 0", got2)
	}
}

func TestFilterModels(t *testing.T) {
	models := []api.OpenAIModel{
		{ID: "deepseek/deepseek-v4-pro", OwnedBy: "command-code"}, // open (has /)
		{ID: "claude-sonnet-4-6", OwnedBy: "command-code"},        // closed (no /)
		{ID: "Qwen/Qwen3.7-Max", OwnedBy: "command-code"},         // open (has /)
		{ID: "gpt-5.5", OwnedBy: "command-code"},                  // closed (no /)
	}

	openOnly := filterModels(models, false)
	if len(openOnly) != 2 {
		t.Fatalf("expected 2 open models, got %d", len(openOnly))
	}
	if openOnly[0].ID != "deepseek/deepseek-v4-pro" {
		t.Errorf("open[0].ID = %q, want deepseek/deepseek-v4-pro", openOnly[0].ID)
	}
	if openOnly[1].ID != "Qwen/Qwen3.7-Max" {
		t.Errorf("open[1].ID = %q, want Qwen/Qwen3.7-Max", openOnly[1].ID)
	}

	all := filterModels(models, true)
	if len(all) != 4 {
		t.Fatalf("expected 4 models when includeClosed=true, got %d", len(all))
	}
}

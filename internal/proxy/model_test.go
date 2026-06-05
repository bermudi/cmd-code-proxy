package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/bermudi/cmd-code-proxy/internal/api"
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

func TestProjectSlugFromPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/Users/patwoz/dev/Personal/pi/pi-commandcode-provider", "users-patwoz-dev-personal-pi-pi-commandcode-provider"},
		{"/repo", "repo"},
		{"C:\\Users\\Pat\\Project", "users-pat-project"},
		{"///", "project"},
	}
	for _, c := range cases {
		got := projectSlugFromPath(c.in)
		if got != c.want {
			t.Errorf("projectSlugFromPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildRequest_UsesCLICompatibleContext(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	body, err := BuildCCRequest(api.OpenAIChatRequest{
		Model: "deepseek-v4-flash",
		Messages: []api.OpenAIMessage{
			{Role: "user", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}

	if body.Config.WorkingDir != tmp {
		t.Errorf("WorkingDir = %q, want %q", body.Config.WorkingDir, tmp)
	}
	if body.Config.Environment != "cli" {
		t.Errorf("Environment = %q, want cli", body.Config.Environment)
	}
	if body.Config.MainBranch != "" {
		t.Errorf("MainBranch = %q, want empty string", body.Config.MainBranch)
	}

	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for _, key := range []string{"memory", "taste", "skills"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("%s missing from request JSON: %s", key, data)
		}
		if raw[key] != nil {
			t.Errorf("%s = %#v, want JSON null", key, raw[key])
		}
	}
}

func TestCreateUpstreamRequest_SetsCLIHeaders(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	body, err := BuildCCRequest(api.OpenAIChatRequest{
		Model: "deepseek-v4-flash",
		Messages: []api.OpenAIMessage{
			{Role: "user", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}

	a := &ccAdapter{baseURL: "https://example.test"}
	req, err := a.createUpstreamRequest(context.Background(), body, "test-key")
	if err != nil {
		t.Fatalf("CreateUpstreamRequest() error = %v", err)
	}
	defer req.Body.Close()

	if req.Header.Get("x-cli-environment") != "production" {
		t.Errorf("x-cli-environment = %q, want production", req.Header.Get("x-cli-environment"))
	}
	if req.Header.Get("x-project-slug") != projectSlugFromPath(tmp) {
		t.Errorf("x-project-slug = %q, want %q", req.Header.Get("x-project-slug"), projectSlugFromPath(tmp))
	}
	if req.Header.Get("x-taste-learning") != "true" {
		t.Errorf("x-taste-learning = %q, want true", req.Header.Get("x-taste-learning"))
	}
	if req.Header.Get("x-co-flag") != "false" {
		t.Errorf("x-co-flag = %q, want false", req.Header.Get("x-co-flag"))
	}

	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(reqBody, &raw); err != nil {
		t.Fatalf("Unmarshal(request body) error = %v", err)
	}
	if raw["memory"] != nil || raw["taste"] != nil || raw["skills"] != nil {
		t.Errorf("memory/taste/skills = %#v/%#v/%#v, want null/null/null", raw["memory"], raw["taste"], raw["skills"])
	}
}

// Package paritytest — request-shape assertions for the proxy vs the real
// command-code CLI binary.
//
// The captures in /home/daniel/build/cmd-recorder/captures/ are ground
// truth: real /alpha/generate bodies from the real binary, mid-agentic
// loop. The proxy must produce equivalent bodies given an OpenAI-shaped
// request from pi. The three failures these tests guard against:
//
//  1. A system message in params.messages[]. The real binary never sends
//     one; CommandCode's gateway builds the system prompt server-side
//     from config.workingDir. If the proxy forwards the OpenAI system
//     message (e.g. pi's harness, which bakes the project AGENTS.md into
//     the system message), the model sees a fake "user" turn that looks
//     like an environment announcement and hallucinates an
//     acknowledgement — the "Working directory: … Ready for your next
//     request." pattern.
//
//  2. A user message whose content opens with the AGENTS.md markdown
//     headers. This is the visible symptom of (1) — the rewrite to
//     role: "user" leaks through.
//
//  3. config.workingDir not set to the project path. Without it the
//     gateway can't find the project to read AGENTS.md from.
package paritytest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bermudi/cmd-code-proxy/internal/api"
	"github.com/bermudi/cmd-code-proxy/internal/proxy"
)

// loadRealBinaryCapture returns one real /alpha/generate body from the
// command-code binary's recorder. The captures are the ground truth; if
// the directory is gone, the test is skipped and the next person who
// breaks the shape has to re-record with the real binary.
func loadRealBinaryCapture(t *testing.T) map[string]any {
	t.Helper()
	const captureDir = "/home/daniel/build/cmd-recorder/captures"
	info, err := os.Stat(captureDir)
	if err != nil || !info.IsDir() {
		t.Skipf("cmd-recorder captures not found at %s; skipping real-binary parity check", captureDir)
	}
	entries, err := os.ReadDir(captureDir)
	if err != nil {
		t.Fatalf("read %s: %v", captureDir, err)
	}
	var chosen string
	for i := len(entries) - 1; i >= 0; i-- {
		name := entries[i].Name()
		if strings.Contains(name, "POST_alpha_generate") && strings.HasSuffix(name, ".json") {
			chosen = filepath.Join(captureDir, name)
			break
		}
	}
	if chosen == "" {
		t.Skip("no POST /alpha/generate captures in cmd-recorder; skipping")
	}
	data, err := os.ReadFile(chosen)
	if err != nil {
		t.Fatalf("read %s: %v", chosen, err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("unmarshal %s: %v", chosen, err)
	}
	return body
}

// TestCommandCodeShape_NoSystemRoleInMessages — the real binary never
// puts a system or developer role in params.messages[]. Assert the
// captures confirm this, so a future "let me just add it because pi
// sends it" change in the proxy has to explain itself.
func TestCommandCodeShape_NoSystemRoleInMessages(t *testing.T) {
	realBody := loadRealBinaryCapture(t)

	params, ok := realBody["params"].(map[string]any)
	if !ok {
		t.Fatal("real body missing params")
	}
	msgs, ok := params["messages"].([]any)
	if !ok {
		t.Fatal("real body missing params.messages")
	}
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" || role == "developer" {
			t.Errorf("real binary message[%d].role = %q; command-code never sends system/developer in params.messages", i, role)
		}
	}
}

// TestProxyRequestShape_DropsSystemMessages — given a pi-shaped OpenAI
// request whose system message contains the project's AGENTS.md, the
// proxy's CCRequestBody must not contain a single system-role message
// in params.messages[].
func TestProxyRequestShape_DropsSystemMessages(t *testing.T) {
	openAIReq := api.OpenAIChatRequest{
		Model: "test-model",
		Messages: []api.OpenAIMessage{
			// Pi's harness bakes the project AGENTS.md into this system message.
			{Role: "system", Content: "# AGENTS.md\n\nagents.md standard. Global file — project-level AGENTS.md overrides this."},
			{Role: "system", Content: "You are an expert coding assistant operating inside pi."},
			{Role: "user", Content: "hello there"},
			{Role: "assistant", Content: "Hey! What are we working on?"},
			{Role: "user", Content: "nothing yet"},
		},
		Stream: true,
	}

	body, err := proxy.BuildCCRequest(openAIReq)
	if err != nil {
		t.Fatalf("BuildCCRequest: %v", err)
	}

	for i, m := range body.Params.Messages {
		if m.Role == "system" || m.Role == "developer" {
			t.Errorf("proxy emitted message[%d] with role %q; system/developer must be dropped", i, m.Role)
		}
	}

	// The 3 non-system input messages (user, assistant, user) should
	// all survive in order. If the count is off, something is dropping
	// user/assistant turns by mistake.
	if len(body.Params.Messages) != 3 {
		t.Errorf("len(messages) = %d, want 3 (system x2 dropped, user/assistant/user kept)", len(body.Params.Messages))
	}
}

// TestProxyRequestShape_NoAgentsMDLeakage — the visible symptom of "system
// not dropped" is a user turn whose content opens with the AGENTS.md
// markdown headers. This covers both the rewrite-to-user regression and
// any future "extract and re-inject" attempt that forgets to strip
// headers.
func TestProxyRequestShape_NoAgentsMDLeakage(t *testing.T) {
	openAIReq := api.OpenAIChatRequest{
		Model: "test-model",
		Messages: []api.OpenAIMessage{
			{Role: "system", Content: "# AGENTS.md\n\n[agents.md](https://agents.md/) standard. ...\n# Project-specific instructions and guidelines:"},
			{Role: "user", Content: "hello"},
		},
		Stream: true,
	}

	body, err := proxy.BuildCCRequest(openAIReq)
	if err != nil {
		t.Fatalf("BuildCCRequest: %v", err)
	}

	for i, m := range body.Params.Messages {
		text := firstText(m.Content)
		if strings.HasPrefix(text, "# AGENTS.md") || strings.HasPrefix(text, "# Project-specific instructions") {
			t.Errorf("proxy emitted message[%d] (role=%q) whose content opens with the AGENTS.md header — system content leaked into a user turn: %q", i, m.Role, preview(text, 80))
		}
	}
}

// TestProxyRequestShape_ConfigWorkingDirSet — without config.workingDir
// the gateway can't locate the project to read AGENTS.md from. The
// proxy must populate it on every request, even if there's no per-request
// override and no flag.
func TestProxyRequestShape_ConfigWorkingDirSet(t *testing.T) {
	body, err := proxy.BuildCCRequest(api.OpenAIChatRequest{
		Model:    "test-model",
		Messages: []api.OpenAIMessage{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("BuildCCRequest: %v", err)
	}
	if body.Config.WorkingDir == "" {
		t.Error("Config.WorkingDir is empty; gateway needs it to find the project and read AGENTS.md")
	}
}

// firstText pulls the first text part out of a CC content-part slice. The
// proxy's CCContentPart has Text as a *string; we treat a nil pointer as
// the empty string.
func firstText(content []api.CCContentPart) string {
	for _, p := range content {
		if p.Type == "text" && p.Text != nil {
			return *p.Text
		}
	}
	return ""
}

func preview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

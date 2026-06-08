package proxy

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bermudi/cmd-code-proxy/internal/api"
)

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
		case strings.HasPrefix(line, "A "):
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

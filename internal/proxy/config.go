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

// populateConfigFromFS impersonates the real command-code binary's config
// population. Every field must match what command-code would send — this
// is not the proxy's environment, it is the proxy pretending to be
// command-code. Fields that come from the project directory (structure,
// git status, branch) are read from workingDir; the Environment string is
// a static lie that matches the real binary's output.
//
// Previously the proxy sent stubs (empty structure, isGitRepo=false,
// "Go proxy" environment), which made the server-side system prompt look
// like a generic environment announcement and tripped MiniMax-M3's prior
// for treating short input as system state.
//
// workingDir MUST be the project's working directory (from the cc-cwd
// extension or the -working-dir flag), NOT os.Getwd() — the proxy
// process runs from its own checkout dir. Reading the proxy's cwd would
// leak the proxy's own tree (go.mod, internal/, etc.) into the gateway's
// system prompt.

// recentCommitCount matches the real command-code binary's default.
const recentCommitCount = 3

func populateConfigFromFS(workingDir string) api.CCConfig {
	cfg := api.CCConfig{
		WorkingDir:  workingDir,
		Date:        time.Now().Format("2006-01-02"),
		Environment: "linux-x64, Node.js v26.2.0", // impersonates command-code CLI v0.32.2 — NOT the proxy's real OS/runtime
		Structure:   readDirNames(workingDir),
	}
	if isGitRepo(workingDir) {
		cfg.IsGitRepo = true
		cfg.CurrentBranch = gitOutput(workingDir, "branch", "--show-current")
		cfg.MainBranch = gitMainBranch(workingDir)
		cfg.GitStatus = gitStatusSummary(workingDir)
		cfg.RecentCommits = gitLogOneline(workingDir, recentCommitCount)
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
// porcelain output as "M N, A N, D N, R N, ?? N" or "Working tree clean".
func gitStatusSummary(dir string) string {
	out := gitOutput(dir, "status", "--porcelain")
	return summarizePorcelain(out)
}

// summarizePorcelain categorizes `git status --porcelain` output into a
// human-readable summary: "M 2, A 1, D 1, R 1, ?? 3" or "Working tree clean".
//
// The XY format: X = index status, Y = worktree status.
// X: [MADRCTU ], Y: [MDTU ], special: ?? (untracked), !! (ignored).
// Unmerged combinations (UU, AA, DD, AU, UA, DU, UD) count as modified.
// Rename (R) is its own category. Copy (C) maps to added. Type-change (T) maps to modified.
func summarizePorcelain(porcelain string) string {
	if porcelain == "" {
		return "Working tree clean"
	}
	lines := strings.Split(porcelain, "\n")
	var modified, added, deleted, renamed, untracked int
	for _, line := range lines {
		if len(line) < 2 {
			continue
		}
		x, y := line[0], line[1]
		switch {
		case x == '?' && y == '?':
			untracked++
		case x == '!' && y == '!':
			// ignored — skip
		case x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D'):
			modified++ // unmerged → treat as modified
		case x == 'R':
			renamed++
		case x == 'C':
			added++ // copy ≈ add
		case x == 'A':
			added++
		case x == 'M' || y == 'M' || x == 'T' || y == 'T':
			modified++
		case x == 'D' || y == 'D':
			deleted++
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
	if renamed > 0 {
		parts = append(parts, fmt.Sprintf("R %d", renamed))
	}
	if untracked > 0 {
		parts = append(parts, fmt.Sprintf("?? %d", untracked))
	}
	if len(parts) == 0 {
		return "Working tree clean"
	}
	return strings.Join(parts, ", ")
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

package proxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// --- Pure function tests (no filesystem/git dependency) ---

func TestSummarizePorcelain(t *testing.T) {
	tests := []struct {
		name      string
		porcelain string
		want      string
	}{
		// --- basics ---
		{"clean", "", "Working tree clean"},
		{"one modified", " M file1.txt", "M 1"},
		{"mixed", " M a.txt\n M b.txt\nA c.txt\n?? d.txt\n?? e.txt", "M 2, A 1, ?? 2"},
		{"only untracked", "?? foo\n?? bar\n?? baz", "?? 3"},

		// --- staged vs unstaged ---
		{"unstaged delete", " D old.txt", "D 1"},
		{"staged add", "A  newfile.txt", "A 1"},
		{"staged modified", "M  staged.txt", "M 1"},
		{"staged deleted", "D  staged-del.txt", "D 1"},
		{"both modified", "MM both.txt", "M 1"},
		{"added then modified", "AM file.txt", "A 1"},

		// --- rename / copy ---
		{"rename", "R  old.txt -> new.txt", "R 1"},
		{"rename then modified", "RM rmod.txt", "R 1"},
		{"copy", "C  orig.txt -> copy.txt", "A 1"},

		// --- type change ---
		{"type change", "T  symlink", "M 1"},

		// --- unmerged (merge conflicts) ---
		{"unmerged both modified", "UU conflict.txt", "M 1"},
		{"unmerged both added", "AA theirs.txt", "M 1"},
		{"unmerged both deleted", "DD ours.txt", "M 1"},
		{"unmerged added-by-us", "AU by-us.txt", "M 1"},
		{"unmerged added-by-them", "UA by-them.txt", "M 1"},
		{"unmerged deleted-by-them", "UD by-them.txt", "M 1"},
		{"unmerged deleted-by-us", "DU del-us.txt", "M 1"},

		// --- ignored ---
		{"ignored", "!! build/", "Working tree clean"},

		// --- combined ---
		{"mixed with rename", " M a.txt\nR  old -> new\n?? c.txt", "M 1, R 1, ?? 1"},
		{"all categories", " M mod.txt\nM  staged.txt\nA  new.txt\nD  del.txt\nR  old -> new\n?? untracked", "M 2, A 1, D 1, R 1, ?? 1"},

		// --- edge cases ---
		{"short line", "X", "Working tree clean"},
		{"empty line in middle", " M a.txt\n\n?? b.txt", "M 1, ?? 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizePorcelain(tt.porcelain)
			if got != tt.want {
				t.Errorf("summarizePorcelain(%q) = %q, want %q", tt.porcelain, got, tt.want)
			}
		})
	}
}

func TestParseMainBranch(t *testing.T) {
	// gitMainBranch parses `git branch -r` output — test the parsing by
	// using gitMainBranch directly on temp repos with remote refs.
	tests := []struct {
		name    string
		remotes []string // branches to create under origin/
		want    string
	}{
		{"origin/main present", []string{"main"}, "main"},
		{"origin/master present", []string{"master"}, "master"},
		{"both present", []string{"main", "master"}, "main"},
		{"no standard branches", []string{"develop"}, "main"},
		{"empty", nil, "main"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			git(t, dir, "init")
			git(t, dir, "config", "user.email", "test@test.com")
			git(t, dir, "config", "user.name", "Test")
			writeFile(t, dir, "file.txt", "hello")
			git(t, dir, "add", ".")
			git(t, dir, "commit", "-m", "initial")

			// Create fake remote refs
			for _, branch := range tt.remotes {
				sha := gitOutput(dir, "rev-parse", "HEAD")
				refDir := filepath.Join(dir, ".git", "refs", "remotes", "origin")
				if err := os.MkdirAll(refDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(refDir, branch), []byte(sha+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			got := gitMainBranch(dir)
			if got != tt.want {
				t.Errorf("gitMainBranch = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Filesystem integration tests ---

func TestReadDirNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"src", "lib", "meta",
		".hidden", ".config",
		"node_modules", "dist", "build", ".git", "out",
	} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readDirNames(dir)
	want := []string{"lib", "meta", "src"}
	if len(got) != len(want) {
		t.Fatalf("readDirNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("readDirNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReadDirNames_NonexistentDir(t *testing.T) {
	got := readDirNames("/nonexistent/path/that/does/not/exist")
	if len(got) != 0 {
		t.Errorf("readDirNames on nonexistent dir = %v, want empty", got)
	}
}

func TestIsGitRepo(t *testing.T) {
	dir := t.TempDir()
	if isGitRepo(dir) {
		t.Error("empty temp dir should not be a git repo")
	}
	git(t, dir, "init")
	if !isGitRepo(dir) {
		t.Error("git-init'd dir should be a git repo")
	}
}

func TestPopulateConfigFromFS_NonGit(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"src", "docs", ".hidden", "node_modules"} {
		os.MkdirAll(filepath.Join(dir, name), 0o755)
	}

	cfg := populateConfigFromFS(dir)

	if cfg.IsGitRepo {
		t.Error("non-git dir should have IsGitRepo=false")
	}
	if cfg.CurrentBranch != "" {
		t.Errorf("non-git dir should have empty CurrentBranch, got %q", cfg.CurrentBranch)
	}
	if len(cfg.RecentCommits) != 0 {
		t.Errorf("non-git dir should have no RecentCommits, got %v", cfg.RecentCommits)
	}
	want := []string{"docs", "src"}
	if len(cfg.Structure) != len(want) {
		t.Fatalf("structure = %v, want %v", cfg.Structure, want)
	}
	for i := range want {
		if cfg.Structure[i] != want[i] {
			t.Errorf("structure[%d] = %q, want %q", i, cfg.Structure[i], want[i])
		}
	}
	if cfg.Environment != "linux-x64, Node.js v26.2.0" {
		t.Errorf("environment = %q, want command-code CLI impersonation string", cfg.Environment)
	}
}

func TestPopulateConfigFromFS_GitRepo(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	git(t, dir, "config", "user.email", "test@test.com")
	git(t, dir, "config", "user.name", "Test")
	writeFile(t, dir, "file.txt", "hello")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "initial")

	cfg := populateConfigFromFS(dir)

	if !cfg.IsGitRepo {
		t.Error("git repo should have IsGitRepo=true")
	}
	if cfg.CurrentBranch == "" {
		t.Error("git repo should have non-empty CurrentBranch")
	}
	if cfg.GitStatus != "Working tree clean" {
		t.Errorf("clean repo should have 'Working tree clean', got %q", cfg.GitStatus)
	}
	if len(cfg.RecentCommits) != 1 {
		t.Errorf("repo with 1 commit should have 1 RecentCommit, got %d", len(cfg.RecentCommits))
	}
}

func TestGitStatusSummary_DirtyStates(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	git(t, dir, "config", "user.email", "test@test.com")
	git(t, dir, "config", "user.name", "Test")

	// Initial commit with two files
	writeFile(t, dir, "committed.txt", "original\nwith some content\nto avoid rename detection\n")
	writeFile(t, dir, "to-delete.txt", "bye\nalso some padding\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "initial")

	// Create dirty states
	writeFile(t, dir, "committed.txt", "modified\nwith some content\nto avoid rename detection\n") // unstaged modify
	writeFile(t, dir, "new.txt", "fresh") // untracked
	git(t, dir, "rm", "to-delete.txt")  // staged delete

	summary := gitStatusSummary(dir)

	if !strings.Contains(summary, "M ") {
		t.Errorf("summary = %q, want 'M' for modified file", summary)
	}
	if !strings.Contains(summary, "?? ") {
		t.Errorf("summary = %q, want '??' for untracked file", summary)
	}
	if !strings.Contains(summary, "D ") {
		t.Errorf("summary = %q, want 'D' for deleted file", summary)
	}
}

func TestGitStatusSummary_Rename(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	git(t, dir, "config", "user.email", "test@test.com")
	git(t, dir, "config", "user.name", "Test")
	writeFile(t, dir, "old.txt", "some content here\nwith enough text\nfor rename detection\nto work reliably\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "initial")

	git(t, dir, "mv", "old.txt", "new.txt")

	summary := gitStatusSummary(dir)
	if !strings.Contains(summary, "R ") {
		t.Errorf("summary after git mv = %q, want 'R' category", summary)
	}
}

func TestGitLogOneline_LimitsToRecentCommitCount(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	git(t, dir, "config", "user.email", "test@test.com")
	git(t, dir, "config", "user.name", "Test")
	for i := 0; i < 5; i++ {
		writeFile(t, dir, "file.txt", string(rune('a'+i)))
		git(t, dir, "add", ".")
		git(t, dir, "commit", "-m", "commit "+string(rune('0'+i+1)))
	}

	commits := gitLogOneline(dir, recentCommitCount)
	if len(commits) != recentCommitCount {
		t.Fatalf("gitLogOneline(recentCommitCount) returned %d commits, want %d", len(commits), recentCommitCount)
	}
	if !strings.Contains(commits[0], "commit 5") {
		t.Errorf("first commit = %q, want 'commit 5' substring", commits[0])
	}
}

// Ensure sort is referenced (used in readDirNames)
var _ = sort.Strings

// helpers

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %s: %v", args, dir, out, err)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

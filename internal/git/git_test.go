package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initTestRepo creates a temporary git repo with one commit and returns its path.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "--initial-branch=main", dir},
		{"git", "-C", dir, "config", "user.email", "test@test.com"},
		{"git", "-C", dir, "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v\n%s", args, err, out)
		}
	}

	// Create a file and initial commit
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", dir, "add", "README.md"},
		{"git", "-C", dir, "commit", "-m", "initial"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v\n%s", args, err, out)
		}
	}

	return dir
}

func TestWorktreeAddRemove(t *testing.T) {
	repo := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "worktree1")

	if err := WorktreeAdd(repo, wtPath, "test-branch", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	// Verify worktree exists
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Errorf("worktree should contain README.md: %v", err)
	}

	if err := WorktreeRemove(repo, wtPath); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}

	// Verify worktree is gone
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree directory should be removed")
	}
}

func TestEnsureDataRef(t *testing.T) {
	repo := initTestRepo(t)
	ref := "refs/klaus/data"

	// First call creates the ref
	if err := EnsureDataRef(repo, ref); err != nil {
		t.Fatalf("EnsureDataRef (create): %v", err)
	}

	// Verify ref exists
	out, err := runGit(repo, "rev-parse", "--verify", ref)
	if err != nil {
		t.Fatalf("ref should exist: %v", err)
	}
	if out == "" {
		t.Error("ref should resolve to a commit")
	}

	// Second call is idempotent
	if err := EnsureDataRef(repo, ref); err != nil {
		t.Fatalf("EnsureDataRef (idempotent): %v", err)
	}
}

func TestSyncToDataRef(t *testing.T) {
	repo := initTestRepo(t)
	ref := "refs/klaus/data"

	// Create a temp file to sync
	tmpFile := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(tmpFile, []byte(`{"id":"test-123"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		"runs/test-123.json": tmpFile,
	}

	if err := SyncToDataRef(repo, ref, "Run test-123", files); err != nil {
		t.Fatalf("SyncToDataRef: %v", err)
	}

	// Verify the file is in the data ref tree
	out, err := runGit(repo, "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	if out != "runs/test-123.json" {
		t.Errorf("ls-tree = %q, want 'runs/test-123.json'", out)
	}

	// Verify content
	content, err := runGit(repo, "show", ref+":runs/test-123.json")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if content != `{"id":"test-123"}` {
		t.Errorf("content = %q, want '{\"id\":\"test-123\"}'", content)
	}
}

func TestSyncToDataRefMultipleFiles(t *testing.T) {
	repo := initTestRepo(t)
	ref := "refs/klaus/data"

	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.json")
	f2 := filepath.Join(dir, "b.jsonl")
	os.WriteFile(f1, []byte("state"), 0o644)
	os.WriteFile(f2, []byte("log"), 0o644)

	files := map[string]string{
		"runs/a.json": f1,
		"logs/a.jsonl": f2,
	}

	if err := SyncToDataRef(repo, ref, "Run a", files); err != nil {
		t.Fatalf("SyncToDataRef: %v", err)
	}

	out, err := runGit(repo, "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}

	lines := filepath.SplitList(out)
	// Just check both files are listed
	if !containsLine(out, "runs/a.json") || !containsLine(out, "logs/a.jsonl") {
		t.Errorf("ls-tree = %q, want both runs/a.json and logs/a.jsonl", lines)
	}
}

func containsLine(output, target string) bool {
	for _, line := range splitLines(output) {
		if line == target {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	for _, line := range filepath.SplitList(s) {
		if line != "" {
			lines = append(lines, line)
		}
	}
	// filepath.SplitList uses OS path separator, we need newlines
	result := []string{}
	start := 0
	for i, c := range s {
		if c == '\n' {
			if i > start {
				result = append(result, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		result = append(result, s[start:])
	}
	return result
}

package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func strp(s string) *string { return &s }

// write drops a file with content under dir, failing the test on error.
func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const brokenGo = "package main\nfunc main() { this is not valid go }\n"
const validGo = "package main\nfunc main() {}\n"
const goMod = "module verifytest\n\ngo 1.21\n"

// TestVerifyRebasedWorktree exercises the real post-rebase verification: it must
// NOT run go build in a non-Go worktree (no go.mod), MUST run go build when a
// go.mod is present, and MUST run a configured command when set.
func TestVerifyRebasedWorktree(t *testing.T) {
	t.Run("no go.mod, no command → skip (non-Go repo merges cleanly)", func(t *testing.T) {
		dir := t.TempDir()
		// A broken .go file is present but must be ignored — no go.mod means skip.
		write(t, dir, "main.go", brokenGo)
		if err := verifyRebasedWorktree(dir, nil); err != nil {
			t.Fatalf("expected skip (nil), got %v", err)
		}
	})

	t.Run("go.mod present → go build runs and catches breakage", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "go.mod", goMod)
		write(t, dir, "main.go", brokenGo)
		if err := verifyRebasedWorktree(dir, nil); err == nil {
			t.Fatal("expected go build to fail on broken source, got nil")
		}
	})

	t.Run("go.mod present + valid source → passes", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "go.mod", goMod)
		write(t, dir, "main.go", validGo)
		if err := verifyRebasedWorktree(dir, nil); err != nil {
			t.Fatalf("expected pass, got %v", err)
		}
	})

	t.Run("configured command runs (success)", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "ran")
		if err := verifyRebasedWorktree(dir, strp("touch ran")); err != nil {
			t.Fatalf("expected pass, got %v", err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("configured command did not run in worktree: %v", err)
		}
	})

	t.Run("configured command runs (failure surfaces)", func(t *testing.T) {
		dir := t.TempDir()
		if err := verifyRebasedWorktree(dir, strp("exit 3")); err == nil {
			t.Fatal("expected configured command failure to surface, got nil")
		}
	})

	t.Run("configured command takes precedence over go build", func(t *testing.T) {
		dir := t.TempDir()
		// Broken go.mod source would fail go build; the command must win and pass.
		write(t, dir, "go.mod", goMod)
		write(t, dir, "main.go", brokenGo)
		if err := verifyRebasedWorktree(dir, strp("true")); err != nil {
			t.Fatalf("expected configured command to take precedence and pass, got %v", err)
		}
	})
}

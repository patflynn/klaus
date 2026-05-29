package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFinalizeCommitAndPush exercises the real git commands the klaus
// finalize command runs: 'git add -A', commit, and push to a local bare
// remote. This is an integration test — no mocks — to catch regressions
// in how WIP is preserved.
func TestFinalizeCommitAndPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	dir := t.TempDir()

	// Bare repo used as the 'origin' remote.
	bare := filepath.Join(dir, "bare.git")
	runGit(t, dir, "init", "--bare", bare)

	// Working clone with an initial commit on main.
	workDir := filepath.Join(dir, "work")
	runGit(t, dir, "clone", bare, workDir)
	configGitIdentity(t, workDir)
	writeFile(t, filepath.Join(workDir, "README.md"), "# init\n")
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "init")
	runGit(t, workDir, "branch", "-M", "main")
	runGit(t, workDir, "push", "-u", "origin", "main")

	// Create the agent's branch with one staged-and-committed change plus
	// an uncommitted change (the "WIP" the agent never got to commit).
	runGit(t, workDir, "checkout", "-b", "agent/test-branch")
	writeFile(t, filepath.Join(workDir, "wip.txt"), "uncommitted edits\n")

	t.Run("commits uncommitted changes", func(t *testing.T) {
		ctx := context.Background()
		committed, err := commitWorkInProgress(ctx, workDir, "WIP from klaus")
		if err != nil {
			t.Fatalf("commitWorkInProgress: %v", err)
		}
		if !committed {
			t.Fatal("expected commit to be created for dirty worktree")
		}

		// Run a second time on a clean worktree — should be a no-op.
		committed2, err := commitWorkInProgress(ctx, workDir, "WIP from klaus")
		if err != nil {
			t.Fatalf("commitWorkInProgress (clean): %v", err)
		}
		if committed2 {
			t.Error("expected no commit on clean worktree")
		}
	})

	t.Run("pushes branch to origin", func(t *testing.T) {
		ctx := context.Background()
		if err := pushBranch(ctx, workDir, "agent/test-branch"); err != nil {
			t.Fatalf("pushBranch: %v", err)
		}

		// Verify the branch landed on the bare remote.
		out := runGitOutput(t, bare, "branch", "--list", "agent/test-branch")
		if !strings.Contains(out, "agent/test-branch") {
			t.Errorf("expected agent/test-branch on origin, got: %q", out)
		}
	})
}

func TestFinalizeCommitWithCleanWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	dir := t.TempDir()
	runGit(t, dir, "init", dir)
	configGitIdentity(t, dir)
	writeFile(t, filepath.Join(dir, "f.txt"), "hello")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")

	ctx := context.Background()
	committed, err := commitWorkInProgress(ctx, dir, "WIP")
	if err != nil {
		t.Fatalf("commitWorkInProgress: %v", err)
	}
	if committed {
		t.Error("expected no commit on already-clean worktree")
	}
}

func configGitIdentity(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "config", "user.email", "test@test")
	runGit(t, dir, "config", "user.name", "test")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeFile %s: %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

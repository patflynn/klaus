package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runInRepo is a tiny helper used by these tests to run raw git commands.
func runInRepo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// setupRepoWithRemote creates two bare-ish repos: `origin` (bare) and `repo`
// (a working clone of origin). It returns the clone path.
func setupRepoWithRemote(t *testing.T) string {
	t.Helper()

	upstream := initTestRepo(t)
	bareDir := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "clone", "--bare", upstream, bareDir).CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v\n%s", err, out)
	}

	cloneDir := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", bareDir, cloneDir).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	runInRepo(t, cloneDir, "config", "user.email", "test@test.com")
	runInRepo(t, cloneDir, "config", "user.name", "Test")

	return cloneDir
}

func TestIsClean(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	clean, err := IsClean(ctx, repo)
	if err != nil {
		t.Fatalf("IsClean on fresh repo: %v", err)
	}
	if !clean {
		t.Errorf("fresh repo should be clean")
	}

	// Add an untracked file — still counts as dirty
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, err = IsClean(ctx, repo)
	if err != nil {
		t.Fatalf("IsClean with untracked file: %v", err)
	}
	if clean {
		t.Errorf("repo with untracked file should not be clean")
	}
}

func TestCurrentBranch(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	branch, err := CurrentBranch(ctx, repo)
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "main" {
		t.Errorf("CurrentBranch = %q, want main", branch)
	}

	// Detached HEAD returns empty string without error
	runInRepo(t, repo, "checkout", "--detach")
	branch, err = CurrentBranch(ctx, repo)
	if err != nil {
		t.Fatalf("CurrentBranch detached: %v", err)
	}
	if branch != "" {
		t.Errorf("detached HEAD should return empty branch, got %q", branch)
	}
}

func TestHasUpstream(t *testing.T) {
	ctx := context.Background()

	// Plain repo, no remote — no upstream
	repo := initTestRepo(t)
	up, err := HasUpstream(ctx, repo)
	if err != nil {
		t.Fatalf("HasUpstream no-remote: %v", err)
	}
	if up {
		t.Errorf("repo without remote should have no upstream")
	}

	// Repo cloned from origin — main tracks origin/main
	clone := setupRepoWithRemote(t)
	up, err = HasUpstream(ctx, clone)
	if err != nil {
		t.Fatalf("HasUpstream with upstream: %v", err)
	}
	if !up {
		t.Errorf("cloned repo's main should have an upstream")
	}
}

func TestMergeFastForward(t *testing.T) {
	ctx := context.Background()
	clone := setupRepoWithRemote(t)

	// Advance origin by committing to another clone, then pushing.
	// Easiest path: make a commit in clone, push, then hard-reset clone back
	// one commit to simulate being behind origin.
	if err := os.WriteFile(filepath.Join(clone, "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runInRepo(t, clone, "add", "extra.txt")
	runInRepo(t, clone, "commit", "-m", "advance")
	runInRepo(t, clone, "push", "origin", "main")
	runInRepo(t, clone, "reset", "--hard", "HEAD~1")

	// Before merge: HEAD is behind origin/main by one commit
	if err := FetchAll(ctx, clone); err != nil {
		t.Fatalf("FetchAll: %v", err)
	}

	if err := MergeFastForward(ctx, clone); err != nil {
		t.Fatalf("MergeFastForward: %v", err)
	}

	// After fast-forward, extra.txt should be present again
	if _, err := os.Stat(filepath.Join(clone, "extra.txt")); err != nil {
		t.Errorf("fast-forward should bring extra.txt back: %v", err)
	}
}

func TestMergeFastForward_DivergedFails(t *testing.T) {
	ctx := context.Background()
	clone := setupRepoWithRemote(t)

	// Make and push one commit, then reset locally and make a different commit.
	// After re-fetch, local and origin/main have diverged.
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	runInRepo(t, clone, "add", "a.txt")
	runInRepo(t, clone, "commit", "-m", "a")
	runInRepo(t, clone, "push", "origin", "main")
	runInRepo(t, clone, "reset", "--hard", "HEAD~1")
	if err := os.WriteFile(filepath.Join(clone, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	runInRepo(t, clone, "add", "b.txt")
	runInRepo(t, clone, "commit", "-m", "b")

	if err := FetchAll(ctx, clone); err != nil {
		t.Fatalf("FetchAll: %v", err)
	}

	if err := MergeFastForward(ctx, clone); err == nil {
		t.Errorf("MergeFastForward should fail when diverged")
	}
}

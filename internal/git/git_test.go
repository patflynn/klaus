package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	ctx := context.Background()
	repo := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "worktree1")

	if err := WorktreeAdd(ctx, repo, wtPath, "test-branch", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	// Verify worktree exists
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Errorf("worktree should contain README.md: %v", err)
	}

	if err := WorktreeRemove(ctx, repo, wtPath); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}

	// Verify worktree is gone
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree directory should be removed")
	}
}

func TestCommonDirFromWorktree(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	if err := WorktreeAdd(ctx, repo, wtPath, "test-branch", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	defer WorktreeRemove(ctx, repo, wtPath)

	// Save original dir and chdir to worktree
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	os.Chdir(wtPath)

	// RepoRoot from a worktree returns the worktree path, NOT the main repo
	root, err := RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	if root != wtPath {
		t.Errorf("RepoRoot() = %q, expected worktree path %q", root, wtPath)
	}

	// CommonDir from a worktree returns the main repo's .git dir
	common, err := CommonDir(ctx)
	if err != nil {
		t.Fatalf("CommonDir: %v", err)
	}
	mainRoot := filepath.Dir(common)
	if mainRoot != repo {
		t.Errorf("filepath.Dir(CommonDir()) = %q, expected main repo %q", mainRoot, repo)
	}

	// The main repo root survives worktree removal.
	if err := WorktreeRemove(ctx, repo, wtPath); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}
	if _, err := os.Stat(mainRoot); err != nil {
		t.Errorf("main repo root should still exist after worktree removal: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree should be gone after removal")
	}
}

func TestEnsureDataRef(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	ref := "refs/klaus/data"

	// First call creates the ref
	if err := EnsureDataRef(ctx, repo, ref); err != nil {
		t.Fatalf("EnsureDataRef (create): %v", err)
	}

	// Verify ref exists
	out, err := runGit(ctx, repo, "rev-parse", "--verify", ref)
	if err != nil {
		t.Fatalf("ref should exist: %v", err)
	}
	if out == "" {
		t.Error("ref should resolve to a commit")
	}

	// Second call is idempotent
	if err := EnsureDataRef(ctx, repo, ref); err != nil {
		t.Fatalf("EnsureDataRef (idempotent): %v", err)
	}
}

func TestSyncToDataRef(t *testing.T) {
	ctx := context.Background()
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

	if err := SyncToDataRef(ctx, repo, ref, "Run test-123", files); err != nil {
		t.Fatalf("SyncToDataRef: %v", err)
	}

	// Verify the file is in the data ref tree
	out, err := runGit(ctx, repo, "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	if out != "runs/test-123.json" {
		t.Errorf("ls-tree = %q, want 'runs/test-123.json'", out)
	}

	// Verify content
	content, err := runGit(ctx, repo, "show", ref+":runs/test-123.json")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if content != `{"id":"test-123"}` {
		t.Errorf("content = %q, want '{\"id\":\"test-123\"}'", content)
	}
}

func TestSyncToDataRefMultipleFiles(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	ref := "refs/klaus/data"

	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.json")
	f2 := filepath.Join(dir, "b.jsonl")
	os.WriteFile(f1, []byte("state"), 0o644)
	os.WriteFile(f2, []byte("log"), 0o644)

	files := map[string]string{
		"runs/a.json":  f1,
		"logs/a.jsonl": f2,
	}

	if err := SyncToDataRef(ctx, repo, ref, "Run a", files); err != nil {
		t.Fatalf("SyncToDataRef: %v", err)
	}

	out, err := runGit(ctx, repo, "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}

	lines := filepath.SplitList(out)
	// Just check both files are listed
	if !containsLine(out, "runs/a.json") || !containsLine(out, "logs/a.jsonl") {
		t.Errorf("ls-tree = %q, want both runs/a.json and logs/a.jsonl", lines)
	}
}

// resetProtocolCache resets the sync.Once so protocol detection runs again.
func resetProtocolCache() {
	ghProtocolOnce = sync.Once{}
	ghProtocolSSH = false
}

func TestCloneURL(t *testing.T) {
	defer func() {
		ResetDetectGHProtocol()
		resetProtocolCache()
	}()

	t.Run("https", func(t *testing.T) {
		resetProtocolCache()
		SetDetectGHProtocol(func() bool { return false })
		got := CloneURL("owner", "repo")
		want := "https://github.com/owner/repo.git"
		if got != want {
			t.Errorf("CloneURL() = %q, want %q", got, want)
		}
	})

	t.Run("ssh", func(t *testing.T) {
		resetProtocolCache()
		SetDetectGHProtocol(func() bool { return true })
		got := CloneURL("owner", "repo")
		want := "git@github.com:owner/repo.git"
		if got != want {
			t.Errorf("CloneURL() = %q, want %q", got, want)
		}
	})
}

func TestParseRepoRef(t *testing.T) {
	// Force HTTPS protocol for deterministic test results.
	defer func() {
		ResetDetectGHProtocol()
		resetProtocolCache()
	}()
	resetProtocolCache()
	SetDetectGHProtocol(func() bool { return false })

	tests := []struct {
		input     string
		wantOwner string
		wantRepo  string
		wantURL   string
		wantErr   bool
	}{
		{
			input:     "patflynn/cosmo",
			wantOwner: "patflynn",
			wantRepo:  "cosmo",
			wantURL:   "https://github.com/patflynn/cosmo.git",
		},
		{
			input:     "https://github.com/patflynn/cosmo",
			wantOwner: "patflynn",
			wantRepo:  "cosmo",
			wantURL:   "https://github.com/patflynn/cosmo.git",
		},
		{
			input:     "https://github.com/patflynn/cosmo.git",
			wantOwner: "patflynn",
			wantRepo:  "cosmo",
			wantURL:   "https://github.com/patflynn/cosmo.git",
		},
		{
			input:     "git@github.com:patflynn/cosmo.git",
			wantOwner: "patflynn",
			wantRepo:  "cosmo",
			wantURL:   "https://github.com/patflynn/cosmo.git",
		},
		{
			input:     "https://github.com/patflynn/cosmo/",
			wantOwner: "patflynn",
			wantRepo:  "cosmo",
			wantURL:   "https://github.com/patflynn/cosmo.git",
		},
		{
			input:   "invalid",
			wantErr: true,
		},
		{
			input:   "",
			wantErr: true,
		},
		{
			input:   "/repo",
			wantErr: true,
		},
		{
			input:   "../evil/repo",
			wantErr: true,
		},
		{
			input:   "owner/../etc",
			wantErr: true,
		},
		{
			input:   "https://github.com/../etc/passwd",
			wantErr: true,
		},
		{
			input:   "git@github.com:../evil.git",
			wantErr: true,
		},
		{
			input:     "http://github.com/owner/repo",
			wantOwner: "owner",
			wantRepo:  "repo",
			wantURL:   "https://github.com/owner/repo.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			owner, repo, url, err := ParseRepoRef(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != tt.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tt.wantOwner)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
			if url != tt.wantURL {
				t.Errorf("url = %q, want %q", url, tt.wantURL)
			}
		})
	}
}

func TestParseRepoRefSSH(t *testing.T) {
	defer func() {
		ResetDetectGHProtocol()
		resetProtocolCache()
	}()
	resetProtocolCache()
	SetDetectGHProtocol(func() bool { return true })

	owner, repo, url, err := ParseRepoRef("patflynn/cosmo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "patflynn" || repo != "cosmo" {
		t.Errorf("owner/repo = %s/%s, want patflynn/cosmo", owner, repo)
	}
	want := "git@github.com:patflynn/cosmo.git"
	if url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
}

func TestInstallCommitMsgHook_StripsClaudeAttribution(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt-hook")

	if err := WorktreeAdd(ctx, repo, wtPath, "hook-test", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	defer WorktreeRemove(ctx, repo, wtPath)

	if err := InstallCommitMsgHook(ctx, wtPath); err != nil {
		t.Fatalf("InstallCommitMsgHook: %v", err)
	}

	// Create a file and commit with a Co-Authored-By trailer
	if err := os.WriteFile(filepath.Join(wtPath, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"git", "-C", wtPath, "add", "file.txt"},
		{"git", "-C", wtPath, "commit", "-m", "add file\n\nCo-Authored-By: Claude Sonnet 4 <noreply@anthropic.com>"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Verify the Co-Authored-By line was stripped
	msg, err := runGit(ctx, wtPath, "log", "-1", "--format=%B")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if strings.Contains(msg, "Co-Authored-By") {
		t.Errorf("commit message should not contain Co-Authored-By, got:\n%s", msg)
	}
	if !strings.Contains(msg, "add file") {
		t.Errorf("commit message should still contain the original message, got:\n%s", msg)
	}
}

func TestInstallCommitMsgHook_PreservesHumanCoAuthor(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt-hook-human")

	if err := WorktreeAdd(ctx, repo, wtPath, "hook-human-test", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	defer WorktreeRemove(ctx, repo, wtPath)

	if err := InstallCommitMsgHook(ctx, wtPath); err != nil {
		t.Fatalf("InstallCommitMsgHook: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"git", "-C", wtPath, "add", "file.txt"},
		{"git", "-C", wtPath, "commit", "-m", "add file\n\nCo-Authored-By: Alice Smith <alice@example.com>"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	msg, err := runGit(ctx, wtPath, "log", "-1", "--format=%B")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(msg, "Co-Authored-By: Alice Smith") {
		t.Errorf("commit message should preserve human Co-Authored-By, got:\n%s", msg)
	}
}

func TestInstallCommitMsgHook_NoTrailerUnchanged(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt-hook-clean")

	if err := WorktreeAdd(ctx, repo, wtPath, "hook-clean-test", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	defer WorktreeRemove(ctx, repo, wtPath)

	if err := InstallCommitMsgHook(ctx, wtPath); err != nil {
		t.Fatalf("InstallCommitMsgHook: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"git", "-C", wtPath, "add", "file.txt"},
		{"git", "-C", wtPath, "commit", "-m", "just a normal commit message"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	msg, err := runGit(ctx, wtPath, "log", "-1", "--format=%B")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(msg, "just a normal commit message") {
		t.Errorf("commit message should be unchanged, got:\n%s", msg)
	}
}

func TestInstallCommitMsgHook_StripsMultiplePatterns(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt-hook-multi")

	if err := WorktreeAdd(ctx, repo, wtPath, "hook-multi-test", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	defer WorktreeRemove(ctx, repo, wtPath)

	if err := InstallCommitMsgHook(ctx, wtPath); err != nil {
		t.Fatalf("InstallCommitMsgHook: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	commitMsg := "fix bug\n\nCo-Authored-By: Claude Opus 4 (1M context) <noreply@anthropic.com>\n🤖 Generated with Claude Code"

	for _, args := range [][]string{
		{"git", "-C", wtPath, "add", "file.txt"},
		{"git", "-C", wtPath, "commit", "-m", commitMsg},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	msg, err := runGit(ctx, wtPath, "log", "-1", "--format=%B")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if strings.Contains(msg, "Co-Authored-By") {
		t.Errorf("should strip Co-Authored-By, got:\n%s", msg)
	}
	if strings.Contains(msg, "Claude") {
		t.Errorf("should strip Claude references, got:\n%s", msg)
	}
	if !strings.Contains(msg, "fix bug") {
		t.Errorf("should preserve original message, got:\n%s", msg)
	}
}

func TestWorktreeAdd_PrunesStaleAndRetries(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	// Create a worktree, then delete its directory to make it stale
	stalePath := filepath.Join(t.TempDir(), "stale-wt")
	if err := WorktreeAdd(ctx, repo, stalePath, "stale-branch", "main"); err != nil {
		t.Fatalf("WorktreeAdd (setup): %v", err)
	}
	// Remove the worktree directory without telling git — makes it stale
	if err := os.RemoveAll(stalePath); err != nil {
		t.Fatalf("removing stale worktree dir: %v", err)
	}

	// Now try to create a new worktree on the same branch — should auto-prune and succeed
	newPath := filepath.Join(t.TempDir(), "new-wt")
	if err := WorktreeAdd(ctx, repo, newPath, "stale-branch", "main"); err != nil {
		t.Fatalf("WorktreeAdd should recover from stale worktree: %v", err)
	}
	defer WorktreeRemove(ctx, repo, newPath)

	// Verify the new worktree exists
	if _, err := os.Stat(filepath.Join(newPath, "README.md")); err != nil {
		t.Errorf("new worktree should contain README.md: %v", err)
	}
}

func TestWorktreeAddTrack_PrunesStaleAndRetries(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	// Create a local bare repo as "origin" with a feature branch
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	cmd := exec.Command("git", "clone", "--bare", repo, bareDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v\n%s", err, out)
	}
	if _, err := runGit(ctx, repo, "remote", "add", "origin", bareDir); err != nil {
		runGit(ctx, repo, "remote", "set-url", "origin", bareDir)
	}

	// Create and push a feature branch
	if _, err := runGit(ctx, repo, "branch", "feature-x"); err != nil {
		t.Fatalf("branch: %v", err)
	}
	if _, err := runGit(ctx, repo, "push", "origin", "feature-x"); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Create a worktree tracking origin/feature-x, then make it stale
	stalePath := filepath.Join(t.TempDir(), "stale-track")
	if err := WorktreeAddTrack(ctx, repo, stalePath, "feature-x"); err != nil {
		t.Fatalf("WorktreeAddTrack (setup): %v", err)
	}
	if err := os.RemoveAll(stalePath); err != nil {
		t.Fatalf("removing stale worktree dir: %v", err)
	}

	// Retry should prune and succeed
	newPath := filepath.Join(t.TempDir(), "new-track")
	if err := WorktreeAddTrack(ctx, repo, newPath, "feature-x"); err != nil {
		t.Fatalf("WorktreeAddTrack should recover from stale worktree: %v", err)
	}
	defer WorktreeRemove(ctx, repo, newPath)

	if _, err := os.Stat(filepath.Join(newPath, "README.md")); err != nil {
		t.Errorf("new worktree should contain README.md: %v", err)
	}
}

func TestWorktreeAddTrack_ForceRemovesLiveWorktree(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	// Create a local bare repo as "origin" with a feature branch
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	cmd := exec.Command("git", "clone", "--bare", repo, bareDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v\n%s", err, out)
	}
	if _, err := runGit(ctx, repo, "remote", "add", "origin", bareDir); err != nil {
		runGit(ctx, repo, "remote", "set-url", "origin", bareDir)
	}

	// Create and push a feature branch
	if _, err := runGit(ctx, repo, "branch", "feature-y"); err != nil {
		t.Fatalf("branch: %v", err)
	}
	if _, err := runGit(ctx, repo, "push", "origin", "feature-y"); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Create a worktree tracking origin/feature-y (simulates a previous agent run)
	oldPath := filepath.Join(t.TempDir(), "old-worktree")
	if err := WorktreeAddTrack(ctx, repo, oldPath, "feature-y"); err != nil {
		t.Fatalf("WorktreeAddTrack (setup): %v", err)
	}

	// Directory still exists — simulates incomplete cleanup
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("old worktree should exist: %v", err)
	}

	// Creating a new worktree for the same branch should auto-recover
	newPath := filepath.Join(t.TempDir(), "new-worktree")
	if err := WorktreeAddTrack(ctx, repo, newPath, "feature-y"); err != nil {
		t.Fatalf("WorktreeAddTrack should auto-remove live worktree and succeed: %v", err)
	}
	defer WorktreeRemove(ctx, repo, newPath)

	// Old worktree should be gone
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old worktree directory should have been removed")
	}

	// New worktree should be functional
	if _, err := os.Stat(filepath.Join(newPath, "README.md")); err != nil {
		t.Errorf("new worktree should contain README.md: %v", err)
	}
}

func TestWorktreeAdd_LiveWorktreeReturnsError(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	// Create a worktree that's still live (directory exists)
	livePath := filepath.Join(t.TempDir(), "live-wt")
	if err := WorktreeAdd(ctx, repo, livePath, "live-branch", "main"); err != nil {
		t.Fatalf("WorktreeAdd (setup): %v", err)
	}
	defer WorktreeRemove(ctx, repo, livePath)

	// Try to create another worktree on the same branch — should fail with a clear error
	otherPath := filepath.Join(t.TempDir(), "other-wt")
	err := WorktreeAdd(ctx, repo, otherPath, "live-branch", "main")
	if err == nil {
		t.Fatal("WorktreeAdd should fail when branch is in a live worktree")
	}
	if !strings.Contains(err.Error(), "already checked out in active worktree") {
		t.Errorf("error should mention active worktree, got: %v", err)
	}
	if !strings.Contains(err.Error(), livePath) {
		t.Errorf("error should mention the conflicting path %q, got: %v", livePath, err)
	}
}

func TestWorktreePrune(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	// Create a worktree, delete its directory, then prune
	wtPath := filepath.Join(t.TempDir(), "prune-wt")
	if err := WorktreeAdd(ctx, repo, wtPath, "prune-branch", "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	os.RemoveAll(wtPath)

	if err := WorktreePrune(ctx, repo); err != nil {
		t.Fatalf("WorktreePrune: %v", err)
	}

	// After prune, creating a worktree on the same branch should work
	// (using -B since the branch ref still exists after prune)
	newPath := filepath.Join(t.TempDir(), "after-prune")
	_, err := runGit(ctx, repo, "worktree", "add", newPath, "-B", "prune-branch", "main", "--quiet")
	if err != nil {
		t.Fatalf("worktree add after prune should succeed: %v", err)
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

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestExecClientImplementsClient verifies ExecClient satisfies the Client
// interface by running a representative subset of operations against a real
// git repo.
func TestExecClientImplementsClient(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()

	var c Client = NewExecClient()

	// FetchAll — no remote configured in the test repo, so we expect an error
	// but the method must exist and be callable.
	_ = c.FetchAll(ctx, repo)

	// WorktreeAdd / WorktreeRemove
	wtPath := filepath.Join(t.TempDir(), "client-wt")
	if err := c.WorktreeAdd(ctx, repo, wtPath, "client-branch", "main"); err != nil {
		t.Fatalf("WorktreeAdd via Client: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Errorf("worktree should contain README.md: %v", err)
	}

	if err := c.WorktreeRemove(ctx, repo, wtPath); err != nil {
		t.Fatalf("WorktreeRemove via Client: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree directory should be removed")
	}

	// BranchDelete
	if err := c.BranchDelete(ctx, repo, "client-branch"); err != nil {
		t.Fatalf("BranchDelete via Client: %v", err)
	}

	// EnsureDataRef
	ref := "refs/klaus/client-test"
	if err := c.EnsureDataRef(ctx, repo, ref); err != nil {
		t.Fatalf("EnsureDataRef via Client: %v", err)
	}

	// SyncToDataRef
	tmpFile := filepath.Join(t.TempDir(), "data.json")
	if err := os.WriteFile(tmpFile, []byte(`{"test":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"test/data.json": tmpFile}
	if err := c.SyncToDataRef(ctx, repo, ref, "test sync", files); err != nil {
		t.Fatalf("SyncToDataRef via Client: %v", err)
	}

	// WorktreePrune — should succeed even with nothing to prune
	if err := c.WorktreePrune(ctx, repo); err != nil {
		t.Fatalf("WorktreePrune via Client: %v", err)
	}
}

package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A worker can put a shared file back as it found it before finalize looks.
// The attributes and hooks, which nothing writes in the ordinary course,
// still differ; the config's branch tracking doesn't count.
func TestSharedChangesSeesRestoredFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	common := filepath.Join(dir, ".git")
	attrs := filepath.Join(common, "info", "attributes")
	if err := os.WriteFile(attrs, []byte("*.go text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := SharedSnapshot(ctx, common)

	later := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(attrs, []byte("*.go text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(attrs, later, later); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(common, "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Dir(hook), later, later); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "config", "branch.main.remote", "origin").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}

	if got := fmt.Sprint(SharedChanges(before, SharedSnapshot(ctx, common))); got != "[hooks info/attributes]" {
		t.Fatalf("changed = %s, want [hooks info/attributes]", got)
	}
}

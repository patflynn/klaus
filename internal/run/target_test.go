package run

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadTarget(t *testing.T) {
	dir := t.TempDir()

	// No target initially
	repo, err := LoadTarget(dir)
	if err != nil {
		t.Fatalf("LoadTarget on empty dir: %v", err)
	}
	if repo != "" {
		t.Errorf("expected empty repo, got %q", repo)
	}

	// Save target
	if err := SaveTarget(dir, "owner/repo"); err != nil {
		t.Fatalf("SaveTarget: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(filepath.Join(dir, "target.json")); err != nil {
		t.Fatalf("target.json not on disk: %v", err)
	}

	// Load target
	repo, err = LoadTarget(dir)
	if err != nil {
		t.Fatalf("LoadTarget: %v", err)
	}
	if repo != "owner/repo" {
		t.Errorf("expected %q, got %q", "owner/repo", repo)
	}

	// Overwrite target
	if err := SaveTarget(dir, "other/project"); err != nil {
		t.Fatalf("SaveTarget overwrite: %v", err)
	}
	repo, err = LoadTarget(dir)
	if err != nil {
		t.Fatalf("LoadTarget after overwrite: %v", err)
	}
	if repo != "other/project" {
		t.Errorf("expected %q, got %q", "other/project", repo)
	}
}

func TestClearTarget(t *testing.T) {
	dir := t.TempDir()

	// Clear when no target set — should not error
	if err := ClearTarget(dir); err != nil {
		t.Fatalf("ClearTarget on empty dir: %v", err)
	}

	// Set and clear
	if err := SaveTarget(dir, "owner/repo"); err != nil {
		t.Fatalf("SaveTarget: %v", err)
	}
	if err := ClearTarget(dir); err != nil {
		t.Fatalf("ClearTarget: %v", err)
	}

	// Verify cleared
	repo, err := LoadTarget(dir)
	if err != nil {
		t.Fatalf("LoadTarget after clear: %v", err)
	}
	if repo != "" {
		t.Errorf("expected empty repo after clear, got %q", repo)
	}

	// File should be gone
	if _, err := os.Stat(filepath.Join(dir, "target.json")); !os.IsNotExist(err) {
		t.Error("target.json should not exist after clear")
	}
}

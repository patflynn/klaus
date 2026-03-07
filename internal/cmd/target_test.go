package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patflynn/klaus/internal/run"
)

func TestTargetCommand(t *testing.T) {
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "sessions", "test-session")
	store := run.NewHomeDirStoreFromPath(sessionDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	baseDir := store.BaseDir()

	t.Run("set target", func(t *testing.T) {
		if err := run.SaveTarget(baseDir, "myorg/myrepo"); err != nil {
			t.Fatalf("SaveTarget: %v", err)
		}
		repo, err := run.LoadTarget(baseDir)
		if err != nil {
			t.Fatalf("LoadTarget: %v", err)
		}
		if repo != "myorg/myrepo" {
			t.Errorf("expected myorg/myrepo, got %q", repo)
		}
	})

	t.Run("show target", func(t *testing.T) {
		repo, err := run.LoadTarget(baseDir)
		if err != nil {
			t.Fatalf("LoadTarget: %v", err)
		}
		if repo != "myorg/myrepo" {
			t.Errorf("expected myorg/myrepo, got %q", repo)
		}
	})

	t.Run("clear target", func(t *testing.T) {
		if err := run.ClearTarget(baseDir); err != nil {
			t.Fatalf("ClearTarget: %v", err)
		}
		repo, err := run.LoadTarget(baseDir)
		if err != nil {
			t.Fatalf("LoadTarget: %v", err)
		}
		if repo != "" {
			t.Errorf("expected empty after clear, got %q", repo)
		}
	})
}

func TestTargetCommandIntegration(t *testing.T) {
	// Test the target command end-to-end by setting up a session environment
	// and using the run package directly (same path the command takes).
	tmpHome := t.TempDir()
	sessionID := "test-session-integration"
	sessionDir := filepath.Join(tmpHome, ".klaus", "sessions", sessionID)
	if err := os.MkdirAll(filepath.Join(sessionDir, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sessionDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The target command does:
	// 1. sessionStore() → HomeDirStore via KLAUS_SESSION_ID
	// 2. hds.BaseDir() → session base dir
	// 3. run.SaveTarget/LoadTarget/ClearTarget on that dir

	// Simulate the same flow
	store := run.NewHomeDirStoreFromPath(sessionDir)
	baseDir := store.BaseDir()

	// Initially no target
	repo, err := run.LoadTarget(baseDir)
	if err != nil {
		t.Fatalf("initial LoadTarget: %v", err)
	}
	if repo != "" {
		t.Errorf("expected no target initially, got %q", repo)
	}

	// Set target (the command normalizes via ParseRepoRef, which we test separately)
	if err := run.SaveTarget(baseDir, "owner/myrepo"); err != nil {
		t.Fatalf("SaveTarget: %v", err)
	}

	// Verify it persists
	repo, err = run.LoadTarget(baseDir)
	if err != nil {
		t.Fatalf("LoadTarget after set: %v", err)
	}
	if repo != "owner/myrepo" {
		t.Errorf("expected owner/myrepo, got %q", repo)
	}

	// Verify the file is at the expected path
	targetPath := filepath.Join(sessionDir, "target.json")
	if _, err := os.Stat(targetPath); err != nil {
		t.Errorf("target.json not found at %s: %v", targetPath, err)
	}

	// Clear
	if err := run.ClearTarget(baseDir); err != nil {
		t.Fatalf("ClearTarget: %v", err)
	}

	// Verify cleared
	repo, err = run.LoadTarget(baseDir)
	if err != nil {
		t.Fatalf("LoadTarget after clear: %v", err)
	}
	if repo != "" {
		t.Errorf("expected empty after clear, got %q", repo)
	}

	// File should be removed
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Errorf("target.json should be removed after clear")
	}
}

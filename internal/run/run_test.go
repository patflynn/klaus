package run

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestGenID(t *testing.T) {
	id, err := GenID()
	if err != nil {
		t.Fatalf("GenID() error: %v", err)
	}

	// Format: YYYYMMDD-HHMM-XXXX (4 hex chars)
	pattern := `^\d{8}-\d{4}-[0-9a-f]{4}$`
	matched, err := regexp.MatchString(pattern, id)
	if err != nil {
		t.Fatalf("regexp error: %v", err)
	}
	if !matched {
		t.Errorf("GenID() = %q, want format YYYYMMDD-HHMM-XXXX", id)
	}
}

func TestGenIDUniqueness(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 5; i++ {
		id, err := GenID()
		if err != nil {
			t.Fatalf("GenID() error: %v", err)
		}
		if ids[id] {
			t.Errorf("GenID() produced duplicate: %s", id)
		}
		ids[id] = true
	}
}

func TestGitDirStoreSaveLoadRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewGitDirStore(tmpDir)

	issue := "42"
	pane := "%5"
	budget := "5.00"
	logFile := "/tmp/test.jsonl"
	cost := 3.42
	dur := int64(45000)
	prURL := "https://github.com/org/repo/pull/123"

	original := &State{
		ID:         "20260210-1430-a3f2",
		Prompt:     "Add bluetooth config",
		Issue:      &issue,
		Branch:     "agent/20260210-1430-a3f2",
		Worktree:   "/tmp/klaus-sessions/20260210-1430-a3f2",
		TmuxPane:   &pane,
		Budget:     &budget,
		LogFile:    &logFile,
		CreatedAt:  "2026-02-10T14:30:00-08:00",
		CostUSD:    &cost,
		DurationMS: &dur,
		PRURL:      &prURL,
	}

	if err := store.Save(original); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := store.Load(original.ID)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if loaded.ID != original.ID {
		t.Errorf("ID = %q, want %q", loaded.ID, original.ID)
	}
	if loaded.Prompt != original.Prompt {
		t.Errorf("Prompt = %q, want %q", loaded.Prompt, original.Prompt)
	}
	if *loaded.Issue != *original.Issue {
		t.Errorf("Issue = %q, want %q", *loaded.Issue, *original.Issue)
	}
	if loaded.Branch != original.Branch {
		t.Errorf("Branch = %q, want %q", loaded.Branch, original.Branch)
	}
	if *loaded.CostUSD != *original.CostUSD {
		t.Errorf("CostUSD = %f, want %f", *loaded.CostUSD, *original.CostUSD)
	}
	if *loaded.PRURL != *original.PRURL {
		t.Errorf("PRURL = %q, want %q", *loaded.PRURL, *original.PRURL)
	}
}

func TestGitDirStoreSaveLoadNullFields(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewGitDirStore(tmpDir)

	original := &State{
		ID:        "20260210-1430-b1c2",
		Prompt:    "Fix something",
		Branch:    "agent/20260210-1430-b1c2",
		Worktree:  "/tmp/klaus-sessions/20260210-1430-b1c2",
		CreatedAt: "2026-02-10T14:30:00-08:00",
	}

	if err := store.Save(original); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := store.Load(original.ID)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if loaded.Issue != nil {
		t.Errorf("Issue = %v, want nil", loaded.Issue)
	}
	if loaded.TmuxPane != nil {
		t.Errorf("TmuxPane = %v, want nil", loaded.TmuxPane)
	}
	if loaded.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil", loaded.CostUSD)
	}
	if loaded.PRURL != nil {
		t.Errorf("PRURL = %v, want nil", loaded.PRURL)
	}
}

func TestGitDirStoreList(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewGitDirStore(tmpDir)

	states := []*State{
		{ID: "20260210-1430-aaaa", Prompt: "first", Branch: "b1", Worktree: "/tmp/1", CreatedAt: "2026-02-10T14:30:00Z"},
		{ID: "20260210-1500-bbbb", Prompt: "second", Branch: "b2", Worktree: "/tmp/2", CreatedAt: "2026-02-10T15:00:00Z"},
		{ID: "20260210-1200-cccc", Prompt: "third", Branch: "b3", Worktree: "/tmp/3", CreatedAt: "2026-02-10T12:00:00Z"},
	}

	for _, s := range states {
		if err := store.Save(s); err != nil {
			t.Fatalf("Save() error: %v", err)
		}
	}

	result, err := store.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	if len(result) != 3 {
		t.Fatalf("List() returned %d items, want 3", len(result))
	}

	// Should be sorted newest first
	if result[0].ID != "20260210-1500-bbbb" {
		t.Errorf("result[0].ID = %q, want 20260210-1500-bbbb", result[0].ID)
	}
	if result[2].ID != "20260210-1200-cccc" {
		t.Errorf("result[2].ID = %q, want 20260210-1200-cccc", result[2].ID)
	}
}

func TestGitDirStoreListEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewGitDirStore(tmpDir)
	result, err := store.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("List() returned %d items, want 0", len(result))
	}
}

func TestGitDirStoreDelete(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewGitDirStore(tmpDir)

	s := &State{
		ID:        "20260210-1430-dddd",
		Prompt:    "delete me",
		Branch:    "b1",
		Worktree:  "/tmp/1",
		CreatedAt: "2026-02-10T14:30:00Z",
	}

	if err := store.Save(s); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Verify file exists
	path := filepath.Join(store.StateDir(), s.ID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file should exist: %v", err)
	}

	if err := store.Delete(s.ID); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	// Verify file is gone
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("state file should not exist after Delete()")
	}
}

func TestGitDirStoreEnsureDirs(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewGitDirStore(tmpDir)

	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error: %v", err)
	}

	// Verify both directories were created
	if _, err := os.Stat(store.StateDir()); err != nil {
		t.Errorf("StateDir should exist: %v", err)
	}
	if _, err := os.Stat(store.LogDir()); err != nil {
		t.Errorf("LogDir should exist: %v", err)
	}
}

func TestGitDirStoreDirPaths(t *testing.T) {
	store := NewGitDirStore("/repo/.git")
	if got := store.StateDir(); got != filepath.Join("/repo/.git", "klaus", "runs") {
		t.Errorf("StateDir() = %q, want /repo/.git/klaus/runs", got)
	}
	if got := store.LogDir(); got != filepath.Join("/repo/.git", "klaus", "logs") {
		t.Errorf("LogDir() = %q, want /repo/.git/klaus/logs", got)
	}
}

func TestGitDirStoreListSkipsCorruptFiles(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewGitDirStore(tmpDir)

	// Save a valid state
	valid := &State{
		ID:        "20260210-1430-aaaa",
		Prompt:    "valid",
		Branch:    "b1",
		Worktree:  "/tmp/1",
		CreatedAt: "2026-02-10T14:30:00Z",
	}
	if err := store.Save(valid); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Write a corrupt file
	corrupt := filepath.Join(store.StateDir(), "20260210-1430-bbbb.json")
	if err := os.WriteFile(corrupt, []byte("not json"), 0o644); err != nil {
		t.Fatalf("writing corrupt file: %v", err)
	}

	result, err := store.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("List() returned %d items, want 1 (skipping corrupt)", len(result))
	}
	if result[0].ID != "20260210-1430-aaaa" {
		t.Errorf("result[0].ID = %q, want 20260210-1430-aaaa", result[0].ID)
	}
}

func TestIsStale(t *testing.T) {
	oldGrace := StaleGracePeriod
	defer func() { StaleGracePeriod = oldGrace }()
	StaleGracePeriod = 0 // disable grace period for most sub-tests

	paneGone := func(string) bool { return false }
	paneAlive := func(string) bool { return true }

	strp := func(s string) *string { return &s }
	fptr := func(f float64) *float64 { return &f }
	iptr := func(i int64) *int64 { return &i }

	past := time.Now().Add(-10 * time.Minute).Format(time.RFC3339)

	tests := []struct {
		name     string
		state    State
		paneFunc func(string) bool
		want     bool
	}{
		{
			name: "unfinalized, pane gone, old enough => stale",
			state: State{
				ID:        "test-1",
				TmuxPane:  strp("%1"),
				CreatedAt: past,
			},
			paneFunc: paneGone,
			want:     true,
		},
		{
			name: "finalized with cost => not stale",
			state: State{
				ID:        "test-2",
				TmuxPane:  strp("%1"),
				CostUSD:   fptr(0.5),
				CreatedAt: past,
			},
			paneFunc: paneGone,
			want:     false,
		},
		{
			name: "finalized with duration => not stale",
			state: State{
				ID:         "test-3",
				TmuxPane:   strp("%1"),
				DurationMS: iptr(1000),
				CreatedAt:  past,
			},
			paneFunc: paneGone,
			want:     false,
		},
		{
			name: "pane still alive => not stale",
			state: State{
				ID:        "test-4",
				TmuxPane:  strp("%1"),
				CreatedAt: past,
			},
			paneFunc: paneAlive,
			want:     false,
		},
		{
			name: "no tmux pane reference => not stale",
			state: State{
				ID:        "test-5",
				CreatedAt: past,
			},
			paneFunc: paneGone,
			want:     false,
		},
		{
			name: "session type => not stale",
			state: State{
				ID:        "test-6",
				Type:      "session",
				TmuxPane:  strp("%1"),
				CreatedAt: past,
			},
			paneFunc: paneGone,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := TmuxDeps{
				PaneExists: tt.paneFunc,
				PaneIsIdle: func(string) bool { return false },
				PaneIsDead: func(string) bool { return false },
			}
			got := tt.state.IsStaleWith(td)
			if got != tt.want {
				t.Errorf("IsStale() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsStale_GracePeriod(t *testing.T) {
	oldGrace := StaleGracePeriod
	defer func() { StaleGracePeriod = oldGrace }()

	td := TmuxDeps{
		PaneExists: func(string) bool { return false },
		PaneIsIdle: func(string) bool { return false },
		PaneIsDead: func(string) bool { return false },
	}
	StaleGracePeriod = 5 * time.Minute

	strp := func(s string) *string { return &s }

	t.Run("within grace period => not stale", func(t *testing.T) {
		s := State{
			ID:        "test-grace",
			TmuxPane:  strp("%1"),
			CreatedAt: time.Now().Add(-1 * time.Minute).Format(time.RFC3339),
		}
		if s.IsStaleWith(td) {
			t.Error("expected not stale within grace period")
		}
	})

	t.Run("past grace period => stale", func(t *testing.T) {
		s := State{
			ID:        "test-grace-expired",
			TmuxPane:  strp("%1"),
			CreatedAt: time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
		}
		if !s.IsStaleWith(td) {
			t.Error("expected stale past grace period")
		}
	})
}

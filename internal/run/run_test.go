package run

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
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

func TestSaveLoadRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()

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
		Worktree:   "/tmp/klaus/20260210-1430-a3f2",
		TmuxPane:   &pane,
		Budget:     &budget,
		LogFile:    &logFile,
		CreatedAt:  "2026-02-10T14:30:00-08:00",
		CostUSD:    &cost,
		DurationMS: &dur,
		PRURL:      &prURL,
	}

	if err := Save(tmpDir, original); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(tmpDir, original.ID)
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

func TestSaveLoadNullFields(t *testing.T) {
	tmpDir := t.TempDir()

	original := &State{
		ID:        "20260210-1430-b1c2",
		Prompt:    "Fix something",
		Branch:    "agent/20260210-1430-b1c2",
		Worktree:  "/tmp/klaus/20260210-1430-b1c2",
		CreatedAt: "2026-02-10T14:30:00-08:00",
	}

	if err := Save(tmpDir, original); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := Load(tmpDir, original.ID)
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

func TestList(t *testing.T) {
	tmpDir := t.TempDir()

	states := []*State{
		{ID: "20260210-1430-aaaa", Prompt: "first", Branch: "b1", Worktree: "/tmp/1", CreatedAt: "2026-02-10T14:30:00Z"},
		{ID: "20260210-1500-bbbb", Prompt: "second", Branch: "b2", Worktree: "/tmp/2", CreatedAt: "2026-02-10T15:00:00Z"},
		{ID: "20260210-1200-cccc", Prompt: "third", Branch: "b3", Worktree: "/tmp/3", CreatedAt: "2026-02-10T12:00:00Z"},
	}

	for _, s := range states {
		if err := Save(tmpDir, s); err != nil {
			t.Fatalf("Save() error: %v", err)
		}
	}

	result, err := List(tmpDir)
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

func TestListEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	result, err := List(tmpDir)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("List() returned %d items, want 0", len(result))
	}
}

func TestDelete(t *testing.T) {
	tmpDir := t.TempDir()

	s := &State{
		ID:        "20260210-1430-dddd",
		Prompt:    "delete me",
		Branch:    "b1",
		Worktree:  "/tmp/1",
		CreatedAt: "2026-02-10T14:30:00Z",
	}

	if err := Save(tmpDir, s); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Verify file exists
	path := filepath.Join(StateDir(tmpDir), s.ID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file should exist: %v", err)
	}

	if err := Delete(tmpDir, s.ID); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	// Verify file is gone
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("state file should not exist after Delete()")
	}
}

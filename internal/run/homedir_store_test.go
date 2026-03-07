package run

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHomeDirStore_SaveLoadListDelete(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewHomeDirStoreFromPath(tmpDir)

	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	// Verify directories created
	if _, err := os.Stat(store.StateDir()); err != nil {
		t.Fatalf("StateDir not created: %v", err)
	}
	if _, err := os.Stat(store.LogDir()); err != nil {
		t.Fatalf("LogDir not created: %v", err)
	}

	// Save
	state := &State{
		ID:        "20260307-1200-abcd",
		Prompt:    "Fix the bug",
		Branch:    "agent/20260307-1200-abcd",
		Worktree:  "/tmp/worktree",
		CreatedAt: "2026-03-07T12:00:00Z",
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file exists on disk
	stateFile := filepath.Join(store.StateDir(), state.ID+".json")
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("state file not on disk: %v", err)
	}

	// Load
	loaded, err := store.Load(state.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != state.ID {
		t.Errorf("ID mismatch: got %q, want %q", loaded.ID, state.ID)
	}
	if loaded.Prompt != state.Prompt {
		t.Errorf("Prompt mismatch: got %q, want %q", loaded.Prompt, state.Prompt)
	}

	// Save a second state
	state2 := &State{
		ID:        "20260307-1201-bcde",
		Prompt:    "Add tests",
		Branch:    "agent/20260307-1201-bcde",
		Worktree:  "/tmp/worktree2",
		CreatedAt: "2026-03-07T12:01:00Z",
	}
	if err := store.Save(state2); err != nil {
		t.Fatalf("Save second: %v", err)
	}

	// List
	states, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("List: got %d states, want 2", len(states))
	}
	// Sorted by CreatedAt descending
	if states[0].ID != state2.ID {
		t.Errorf("List order: first should be newest, got %q", states[0].ID)
	}

	// Delete
	if err := store.Delete(state.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify deleted
	_, err = store.Load(state.ID)
	if err == nil {
		t.Error("Load after Delete should fail")
	}

	states, err = store.List()
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(states) != 1 {
		t.Errorf("List after delete: got %d states, want 1", len(states))
	}
}

func TestHomeDirStore_LoadNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	_, err := store.Load("nonexistent")
	if err == nil {
		t.Error("Load nonexistent should return error")
	}
}

func TestHomeDirStore_ListEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewHomeDirStoreFromPath(tmpDir)
	// Don't create dirs — should return nil, nil
	states, err := store.List()
	if err != nil {
		t.Fatalf("List on missing dir: %v", err)
	}
	if states != nil {
		t.Errorf("List on missing dir: got %v, want nil", states)
	}
}

func TestHomeDirStore_Paths(t *testing.T) {
	store := NewHomeDirStoreFromPath("/home/user/.klaus/sessions/session-123")
	if got := store.StateDir(); got != "/home/user/.klaus/sessions/session-123/runs" {
		t.Errorf("StateDir: got %q", got)
	}
	if got := store.LogDir(); got != "/home/user/.klaus/sessions/session-123/logs" {
		t.Errorf("LogDir: got %q", got)
	}
	if got := store.BaseDir(); got != "/home/user/.klaus/sessions/session-123" {
		t.Errorf("BaseDir: got %q", got)
	}
}

func TestFindStateInSessions(t *testing.T) {
	tmpHome := t.TempDir()
	sessionsDir := filepath.Join(tmpHome, ".klaus", "sessions")

	// Create two session directories with states
	session1Dir := filepath.Join(sessionsDir, "session-1")
	store1 := NewHomeDirStoreFromPath(session1Dir)
	if err := store1.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	if err := store1.Save(&State{ID: "run-aaa", CreatedAt: "2026-03-07T10:00:00Z"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	session2Dir := filepath.Join(sessionsDir, "session-2")
	store2 := NewHomeDirStoreFromPath(session2Dir)
	if err := store2.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	if err := store2.Save(&State{ID: "run-bbb", CreatedAt: "2026-03-07T11:00:00Z"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Find run-bbb (in session-2)
	state, store, err := FindStateInSessions(tmpHome, "run-bbb")
	if err != nil {
		t.Fatalf("FindStateInSessions: %v", err)
	}
	if state.ID != "run-bbb" {
		t.Errorf("got ID %q, want run-bbb", state.ID)
	}
	if store == nil {
		t.Error("store should not be nil")
	}

	// Not found
	_, _, err = FindStateInSessions(tmpHome, "run-zzz")
	if err == nil {
		t.Error("expected error for nonexistent run")
	}
}

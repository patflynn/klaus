package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/patflynn/klaus/internal/run"
)

func TestCleanupAllSkipsActiveRuns(t *testing.T) {
	store := newFakeStore(
		&run.State{ID: "run-1", Prompt: "a", Branch: "b1", CreatedAt: "2026-01-01T00:00:00Z"},
		&run.State{ID: "run-2", Prompt: "b", Branch: "b2", CreatedAt: "2026-01-01T00:01:00Z"},
		&run.State{ID: "run-3", Prompt: "c", Branch: "b3", CreatedAt: "2026-01-01T00:02:00Z"},
	)

	// Make run-2 active
	origIsRunActive := isRunActive
	defer func() { isRunActive = origIsRunActive }()
	isRunActive = func(s *run.State) bool { return s.ID == "run-2" }

	output := captureStdout(t, func() {
		if err := cleanupAll("", store, false); err != nil {
			t.Fatalf("cleanupAll() error: %v", err)
		}
	})

	// run-2 should be skipped
	if !contains(output, "skipping run-2 (still running)") {
		t.Errorf("expected skip message for run-2, got: %s", output)
	}

	// run-1 and run-3 should be cleaned up
	if _, err := store.Load("run-1"); err == nil {
		t.Error("run-1 should have been deleted")
	}
	if _, err := store.Load("run-2"); err != nil {
		t.Error("run-2 should still exist (was skipped)")
	}
	if _, err := store.Load("run-3"); err == nil {
		t.Error("run-3 should have been deleted")
	}
}

func TestCleanupAllForceRemovesActiveRuns(t *testing.T) {
	store := newFakeStore(
		&run.State{ID: "run-1", Prompt: "a", Branch: "b1", CreatedAt: "2026-01-01T00:00:00Z"},
		&run.State{ID: "run-2", Prompt: "b", Branch: "b2", CreatedAt: "2026-01-01T00:01:00Z"},
	)

	origIsRunActive := isRunActive
	defer func() { isRunActive = origIsRunActive }()
	isRunActive = func(s *run.State) bool { return s.ID == "run-2" }

	output := captureStdout(t, func() {
		if err := cleanupAll("", store, true); err != nil {
			t.Fatalf("cleanupAll() error: %v", err)
		}
	})

	// No skip messages
	if contains(output, "skipping") {
		t.Errorf("expected no skip messages with --force, got: %s", output)
	}

	// Both should be cleaned up
	if _, err := store.Load("run-1"); err == nil {
		t.Error("run-1 should have been deleted")
	}
	if _, err := store.Load("run-2"); err == nil {
		t.Error("run-2 should have been deleted")
	}
}

func TestCleanupOneSkipsActiveRun(t *testing.T) {
	store := newFakeStore(
		&run.State{ID: "run-1", Prompt: "a", Branch: "b1", CreatedAt: "2026-01-01T00:00:00Z"},
	)

	origIsRunActive := isRunActive
	defer func() { isRunActive = origIsRunActive }()
	isRunActive = func(s *run.State) bool { return true }

	output := captureStdout(t, func() {
		if err := cleanupOne("", store, "run-1", false); err != nil {
			t.Fatalf("cleanupOne() error: %v", err)
		}
	})

	if !contains(output, "skipping run-1 (still running)") {
		t.Errorf("expected skip message, got: %s", output)
	}
	if _, err := store.Load("run-1"); err != nil {
		t.Error("run-1 should still exist")
	}
}

func TestCleanupOneForceRemovesActiveRun(t *testing.T) {
	store := newFakeStore(
		&run.State{ID: "run-1", Prompt: "a", Branch: "b1", CreatedAt: "2026-01-01T00:00:00Z"},
	)

	origIsRunActive := isRunActive
	defer func() { isRunActive = origIsRunActive }()
	isRunActive = func(s *run.State) bool { return true }

	captureStdout(t, func() {
		if err := cleanupOne("", store, "run-1", true); err != nil {
			t.Fatalf("cleanupOne() error: %v", err)
		}
	})

	if _, err := store.Load("run-1"); err == nil {
		t.Error("run-1 should have been deleted")
	}
}

func TestIsRunActiveWithSessionEnv(t *testing.T) {
	origIsRunActive := isRunActive
	defer func() { isRunActive = origIsRunActive }()
	// Reset to the real implementation for this test
	isRunActive = origIsRunActive

	t.Setenv(sessionIDEnv, "sess-123")

	// Session run matching current session ID should be active
	s := &run.State{ID: "sess-123", Type: "session"}
	if !isRunActive(s) {
		t.Error("expected session run matching KLAUS_SESSION_ID to be active")
	}

	// Different session ID should not be active (no tmux pane)
	s2 := &run.State{ID: "sess-456", Type: "session"}
	if isRunActive(s2) {
		t.Error("expected different session ID to not be active")
	}

	// Non-session run without tmux pane should not be active
	s3 := &run.State{ID: "sess-123", Type: "launch"}
	if isRunActive(s3) {
		t.Error("expected non-session run without tmux pane to not be active")
	}
}

func TestIsRunActiveWithDashboardPane(t *testing.T) {
	origIsRunActive := isRunActive
	defer func() { isRunActive = origIsRunActive }()
	isRunActive = origIsRunActive

	// A run with no panes should not be active
	s := &run.State{ID: "run-1"}
	if isRunActive(s) {
		t.Error("expected run without panes to not be active")
	}

	// A run with a DashboardPane that doesn't exist should not be active
	fakePaneID := "%999999"
	s2 := &run.State{ID: "run-2", DashboardPane: &fakePaneID}
	if isRunActive(s2) {
		t.Error("expected run with dead dashboard pane to not be active")
	}
}

// fakeStore is an in-memory StateStore for testing.
type fakeStore struct {
	states map[string]*run.State
}

func newFakeStore(states ...*run.State) *fakeStore {
	s := &fakeStore{states: make(map[string]*run.State)}
	for _, st := range states {
		s.states[st.ID] = st
	}
	return s
}

func (f *fakeStore) Save(s *run.State) error     { f.states[s.ID] = s; return nil }
func (f *fakeStore) Load(id string) (*run.State, error) {
	s, ok := f.states[id]
	if !ok {
		return nil, fmt.Errorf("not found: %s", id)
	}
	return s, nil
}
func (f *fakeStore) List() ([]*run.State, error) {
	var out []*run.State
	for _, s := range f.states {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeStore) Delete(id string) error { delete(f.states, id); return nil }
func (f *fakeStore) LogDir() string         { return "" }
func (f *fakeStore) StateDir() string       { return "" }
func (f *fakeStore) EnsureDirs() error      { return nil }

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && bytes.Contains([]byte(s), []byte(substr))
}

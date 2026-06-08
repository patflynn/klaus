//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestCleanupRemovesPaneAndWorktree launches an agent and then cleans it up
// while it is still live, asserting that `klaus cleanup` kills the real pane,
// removes the real worktree, and deletes the state file. The fake claude is
// left blocked so the run is genuinely active (exercising --force).
func TestCleanupRemovesPaneAndWorktree(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	res := h.RunKlaus("launch", "do some work")
	if res.ExitCode != 0 {
		t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	ids := h.RunIDs()
	if len(ids) != 1 {
		t.Fatalf("expected 1 run, got %d: %v", len(ids), ids)
	}
	runID := ids[0]

	st, err := h.ReadState(runID)
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	if st.TmuxPane == nil {
		t.Fatal("launch wrote no TmuxPane")
	}
	pane := *st.TmuxPane
	worktree := st.Worktree

	// Make sure the agent is genuinely running in its pane before cleanup.
	h.WaitForClaudeStart(30 * time.Second)
	if !h.PaneExists(pane) {
		t.Fatalf("pane %s should exist before cleanup", pane)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree should exist before cleanup: %v", err)
	}

	// --force because the blocked stub keeps the run "active".
	res = h.RunKlaus("cleanup", "--force", runID)
	if res.ExitCode != 0 {
		t.Fatalf("cleanup exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	// Pane killed.
	waitPaneGone(t, h, pane, 30*time.Second)

	// Worktree removed.
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed, stat err: %v", err)
	}

	// State file deleted.
	for _, id := range h.RunIDs() {
		if id == runID {
			t.Errorf("state for %s should be deleted after cleanup", runID)
		}
	}
}

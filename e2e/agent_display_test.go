//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestLaunchDetachedAgentPane covers the default agent_display ("detached"):
// the agent gets a real tmux pane for lifecycle management, but that pane lives
// in a separate detached session rather than the coordinator's window, and
// `klaus cleanup --all` takes the session down with the last agent.
func TestLaunchDetachedAgentPane(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	before := h.WindowPanes(h.InitialPane)

	res := h.RunKlaus("launch", "add a health check endpoint")
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
	if st.TmuxPane == nil || *st.TmuxPane == "" {
		t.Fatal("state has no TmuxPane")
	}
	pane := *st.TmuxPane

	h.WaitForClaudeStart(30 * time.Second)

	// The pane is real and alive.
	if !h.PaneExists(pane) {
		t.Fatalf("expected pane %s to exist; panes: %v", pane, h.ListPanes())
	}

	// It lives in the agents session...
	agents := h.AgentsSession()
	if !h.SessionExists(agents) {
		t.Fatalf("expected detached session %s to exist", agents)
	}
	if !contains(h.SessionPanes(agents), pane) {
		t.Errorf("pane %s not in session %s (panes: %v)", pane, agents, h.SessionPanes(agents))
	}

	// ...and nowhere near the coordinator's window, which is untouched.
	after := h.WindowPanes(h.InitialPane)
	if contains(after, pane) {
		t.Errorf("agent pane %s leaked into the coordinator window: %v", pane, after)
	}
	if len(after) != len(before) {
		t.Errorf("coordinator window pane count changed: %v -> %v", before, after)
	}

	// cleanup --all kills the pane and empties out the agents session.
	res = h.RunKlaus("cleanup", "--all", "--force")
	if res.ExitCode != 0 {
		t.Fatalf("cleanup exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	waitPaneGone(t, h, pane, 30*time.Second)

	deadline := time.Now().Add(10 * time.Second)
	for h.SessionExists(agents) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if h.SessionExists(agents) {
		t.Errorf("agents session %s should be gone after cleanup --all (panes: %v)", agents, h.SessionPanes(agents))
	}
}

// TestLaunchPaneDisplayModeSplitsWindow pins the escape hatch: with
// agent_display "pane", the agent still splits the coordinator's window and no
// detached agents session is created.
func TestLaunchPaneDisplayModeSplitsWindow(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.AmendRepoConfig(map[string]any{"agent_display": "pane"})

	res := h.RunKlaus("launch", "add a health check endpoint")
	if res.ExitCode != 0 {
		t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	ids := h.RunIDs()
	if len(ids) != 1 {
		t.Fatalf("expected 1 run, got %d: %v", len(ids), ids)
	}
	st, err := h.ReadState(ids[0])
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	if st.TmuxPane == nil {
		t.Fatal("state has no TmuxPane")
	}
	pane := *st.TmuxPane

	h.WaitForClaudeStart(30 * time.Second)

	if panes := h.WindowPanes(h.InitialPane); !contains(panes, pane) {
		t.Errorf("agent pane %s should be in the coordinator window, got %v", pane, panes)
	}
	if h.SessionExists(h.AgentsSession()) {
		t.Errorf("no detached session should exist in pane mode, but %s does", h.AgentsSession())
	}

	h.ReleaseClaude()
}

// TestLaunchInvalidAgentDisplay asserts a typo in agent_display fails the
// launch up front instead of silently picking a mode.
func TestLaunchInvalidAgentDisplay(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.AmendRepoConfig(map[string]any{"agent_display": "windowed"})

	res := h.RunKlaus("launch", "do the thing")
	if res.ExitCode == 0 {
		t.Fatalf("launch with invalid agent_display exited 0\nstdout:\n%s", res.Stdout)
	}
	for _, want := range []string{"windowed", "detached", "pane"} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("stderr should mention %q, got:\n%s", want, res.Stderr)
		}
	}
	if ids := h.RunIDs(); len(ids) != 0 {
		t.Errorf("no run should have been recorded, got %v", ids)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

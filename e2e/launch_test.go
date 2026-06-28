//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/cmd"
	"github.com/patflynn/klaus/internal/run"
)

// TestLaunchLifecycle is the headline scenario. With a fake claude, a real git
// worktree, and a fake gh, it drives `klaus launch` end-to-end and asserts the
// full lifecycle:
//
//	(a) a real pane is created in the isolated tmux server with the agent title,
//	(b) run state is written with a branch, worktree, and pane,
//	(c) the fake claude was invoked with the expected args,
//	(d) after the agent completes, _finalize populates cost/duration/PR URL from
//	    the stub's emitted stream-json, and
//	(e) the worktree is removed and the pane is killed.
func TestLaunchLifecycle(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	const prompt = "add a health check endpoint"
	res := h.RunKlaus("launch", prompt)
	if res.ExitCode != 0 {
		t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	// Exactly one run should have been recorded.
	ids := h.RunIDs()
	if len(ids) != 1 {
		t.Fatalf("expected 1 run, got %d: %v", len(ids), ids)
	}
	runID := ids[0]

	// (b) State is written with branch/worktree/pane while the agent runs.
	st, err := h.ReadState(runID)
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	if st.Branch == "" {
		t.Error("state has empty Branch")
	}
	if st.Worktree == "" {
		t.Error("state has empty Worktree")
	}
	if st.TmuxPane == nil || *st.TmuxPane == "" {
		t.Fatal("state has no TmuxPane")
	}
	pane := *st.TmuxPane

	// The worktree should actually exist on disk during the run.
	if _, err := os.Stat(st.Worktree); err != nil {
		t.Errorf("worktree should exist during run: %v", err)
	}

	// Wait until the fake claude is actually running in the pane before we
	// assert on the live pane (the stub blocks until released).
	h.WaitForClaudeStart(30 * time.Second)

	// (a) The pane exists in the isolated server with the expected title.
	if !h.PaneExists(pane) {
		t.Fatalf("expected pane %s to exist; panes: %v", pane, h.ListPanes())
	}
	wantTitle := cmd.FormatPaneTitle(runID, "", prompt)
	if got := h.PaneTitle(pane); got != wantTitle {
		t.Errorf("pane title = %q, want %q", got, wantTitle)
	}

	// (c) claude was invoked with the expected args.
	argv := h.ClaudeArgv()
	for _, want := range []string{prompt, "-p", "--output-format", "stream-json", "--max-budget-usd"} {
		if !strings.Contains(argv, want) {
			t.Errorf("claude argv missing %q\n--- argv ---\n%s", want, argv)
		}
	}

	// Let the agent finish; the pipeline runs _format-stream then _finalize.
	h.ReleaseClaude()

	// (d) _finalize populates cost/duration/PR URL from the emitted log.
	final := h.WaitForState(runID, func(s *run.State) bool {
		return s.CostUSD != nil && s.TmuxPane == nil
	}, 30*time.Second)

	if final.CostUSD == nil || *final.CostUSD != 0.1234 {
		t.Errorf("CostUSD = %v, want 0.1234", final.CostUSD)
	}
	if final.DurationMS == nil || *final.DurationMS != 4242 {
		t.Errorf("DurationMS = %v, want 4242", final.DurationMS)
	}
	if final.PRURL == nil || *final.PRURL != "https://github.com/acme/widget/pull/4242" {
		t.Errorf("PRURL = %v, want the PR URL from the stub log", final.PRURL)
	}

	// (e) The worktree is removed and the pane is killed.
	if final.Worktree != "" {
		t.Errorf("Worktree should be cleared after cleanup, got %q", final.Worktree)
	}
	if _, err := os.Stat(st.Worktree); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be removed, stat err: %v", err)
	}
	waitPaneGone(t, h, pane, 30*time.Second)
}

// waitPaneGone polls until the pane disappears from the isolated server.
func waitPaneGone(t *testing.T, h *Harness, pane string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !h.PaneExists(pane) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("pane %s still alive after %s; panes: %v", pane, timeout, h.ListPanes())
}

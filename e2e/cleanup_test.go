//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
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

// TestCleanupDoneKeepsOpenWork runs the real `klaus cleanup --done` over a
// session with a ready change the restacker has made newer versions of, an
// integrated change, a merged PR and a running agent: it removes the
// finished changes' and PR's runs and the ready change's older run, keeps
// the ready change's latest run and the running one, and the session's cost
// is the same after as before.
func TestCleanupDoneKeepsOpenWork(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub()
	h.WriteStub("gh", `#!/usr/bin/env bash
case "$*" in
  *"--json state "*) echo MERGED ;;
esac
exit 0
`)
	sha := func(c byte) string { return strings.Repeat(string(c), 40) }
	ago := func(m int) time.Time { return time.Now().Add(-time.Duration(m) * time.Minute) }

	// The ready change's first run pushed v1, its follow-up v2, and the
	// restacker made v3, the active one.
	older := h.valleyRun("20261013-0900-0001", "first try", "rstk", "Add a health check", 1, sha('1'), ago(30))
	latest := h.valleyRun("20261013-0900-0002", "follow-up", "rstk", "Add a health check", 2, sha('2'), ago(20))
	landed := h.valleyRun("20261013-0900-0003", "fix the build", "lndd", "Fix the build", 1, sha('4'), ago(15))
	landedV := version(1, sha('4'), "Fix the build", "landed")
	landedV["landed_commit"] = sha('4')
	h.writeValleyChanges(map[string]any{
		"schema": "valley-changes/v1",
		"host":   "classic-laddie",
		"repos": []any{map[string]any{
			"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil,
			"changes": []any{
				change(latest, "ready", 3, version(1, sha('1'), "Add a health check", "not-landed"),
					version(2, sha('2'), "Add a health check", "not-landed"),
					version(3, sha('3'), "Add a health check", "not-landed")),
				change(landed, "ready", 1, landedV),
			},
			"branches": []any{},
		}},
	})
	prCost := 2.5
	merged := &run.State{
		ID: "20261013-0900-0004", Type: "launch", Prompt: "ship the docs", TargetRepo: strPtrE2E("acme/widget"),
		Branch: "agent/docs", PRURL: strPtrE2E("https://github.com/acme/widget/pull/41"), CostUSD: &prCost,
		CreatedAt: ago(10).Format(time.RFC3339),
	}
	h.SeedState(merged)

	if res := h.RunKlaus("launch", "do some work"); res.ExitCode != 0 {
		t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	h.WaitForClaudeStart(30 * time.Second)
	var running string
	for _, id := range h.RunIDs() {
		if !strings.HasPrefix(id, "20261013-") {
			running = id
		}
	}
	if running == "" {
		t.Fatalf("no launched run among %v", h.RunIDs())
	}

	sessionCost := func() string {
		t.Helper()
		res := h.RunKlaus("status")
		if res.ExitCode != 0 {
			t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			if strings.HasPrefix(line, "Session cost: ") {
				return strings.Fields(line)[2]
			}
		}
		t.Fatalf("status shows no session cost:\n%s", res.Stdout)
		return ""
	}
	before := sessionCost()

	res := h.RunKlaus("cleanup", "--done")
	if res.ExitCode != 0 {
		t.Fatalf("cleanup --done exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	left := make(map[string]bool)
	for _, id := range h.RunIDs() {
		left[id] = true
	}
	for id, keep := range map[string]bool{older.ID: false, latest.ID: true, landed.ID: false, merged.ID: false, running: true} {
		if left[id] != keep {
			t.Errorf("%s: kept %v, want %v\n%s", id, left[id], keep, res.Stdout)
		}
		verb := "removed "
		if keep {
			verb = "kept    "
		}
		if !strings.Contains(res.Stdout, verb+id+"  ") {
			t.Errorf("cleanup --done should say it %s%s:\n%s", verb, id, res.Stdout)
		}
	}
	if after := sessionCost(); after != before {
		t.Errorf("session cost %s after cleanup --done, want %s as before", after, before)
	}
}

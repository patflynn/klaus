//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
)

// checksListing is a valley-changes/v1 listing of widget with one change,
// c's (handle nxvp), owned by klaus-env and in state at v1. checks is what
// the valley lists for the version's checks; nil lists none, as a valley
// from before checks were listed does.
func checksListing(c strictChange, state string, checks map[string]any) map[string]any {
	v := map[string]any{"n": 1, "head": c.v1, "title": "Add a greeting", "branch": c.branch,
		"uploader": "klaus-env", "landing": "not-landed", "landed_commit": nil, "request": nil, "outcome": nil}
	if checks != nil {
		v["checks"] = checks
	}
	ch := map[string]any{
		"change_id": c.id, "handle": "nxvp", "target": "main", "owner": "klaus-env", "state": state,
		"record": c.record, "seq": 1, "active": 1, "includes": []string{}, "versions": []any{v}, "restack": nil,
	}
	return map[string]any{"schema": "valley-changes/v1", "host": "classic-laddie", "repos": []any{map[string]any{
		"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil, "changes": []any{ch}, "branches": []any{},
	}}}
}

// checks is a version's checks in overall state, with valley-request in
// request and integrator-unit passed.
func checks(state, request, why string) map[string]any {
	vr := map[string]any{"name": "valley-request", "state": request, "signer": "host-worker"}
	if why != "" {
		vr["why"] = why
	}
	return map[string]any{"state": state, "tree": "sha256:" + strings.Repeat("ab", 32), "required": []any{
		vr,
		map[string]any{"name": "integrator-unit", "state": "passed", "signer": "host-worker"},
		map[string]any{"name": "web-unit", "state": "other"},
	}}
}

// serve has the stub valley list state as ready, and as a draft once
// something returns the change to draft, as launch does.
func (h *Harness) serve(c strictChange, chk map[string]any) {
	h.t.Helper()
	os.Remove(filepath.Join(h.E2EDir, "valley.after-unready"))
	h.writeValleyChanges(checksListing(c, "ready", chk))
	h.writeE2EJSON("valley.changes.unready.json", checksListing(c, "draft", nil))
}

// failedChecks runs klaus failed-checks and returns its output and the run
// it launched, if any.
func (h *Harness) failedChecks() (string, *run.State) {
	h.t.Helper()
	known := h.RunIDs()
	res := h.RunKlaus("failed-checks")
	if res.ExitCode != 0 {
		h.t.Fatalf("failed-checks exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	return res.Stdout, h.newRun(known)
}

// newRun returns the run recorded since known, if any, failing the test on
// more than one.
func (h *Harness) newRun(known []string) *run.State {
	h.t.Helper()
	var found *run.State
	for _, id := range h.RunIDs() {
		if slices.Contains(known, id) {
			continue
		}
		if found != nil {
			h.t.Fatalf("more than one run launched: %s and %s", found.ID, id)
		}
		st, err := h.ReadState(id)
		if err != nil {
			h.t.Fatal(err)
		}
		found = st
	}
	return found
}

// TestValleyChecksFailedDispatchesAFix runs the real klaus against a fake
// valley whose listing moves a ready change klaus owns from no checks, to
// pending, to failed, to passed. With no checks listed, status's CI reads
// "-" and nothing is dispatched. The dashboard shows pending checks in the
// CI column. While a run works on the change, a failed head gets no fix
// run. Once it is done, the dashboard reports change:checks-failed once,
// shows the failed check, and launches one fix run on the change, for
// checks-failed, whose brief names the check, the worker's reason and how
// to read its output. The same failed head gets no second run. Passed
// checks are reported once, and the status stays awaiting-approval.
func TestValleyChecksFailedDispatchesAFix(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	c := h.strictChange()
	sessionDir := filepath.Join(h.Home, ".klaus", "sessions", h.SessionID)
	if err := os.WriteFile(filepath.Join(sessionDir, "events.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	checksEvents := func() []event.Event {
		var out []event.Event
		for _, e := range h.runEvents("nxvp") {
			if strings.HasPrefix(e.Type, "change:checks-") {
				out = append(out, e)
			}
		}
		return out
	}
	statusRow := func() string {
		res := h.RunKlaus("status")
		if res.ExitCode != 0 {
			t.Fatalf("status exited %d: %s", res.ExitCode, res.Stderr)
		}
		return res.Stdout
	}

	// No checks listed: nothing to show, nothing to dispatch.
	h.serve(c, nil)
	if out, fix := h.failedChecks(); fix != nil || !strings.Contains(out, "No failed checks") {
		t.Fatalf("with no checks listed, failed-checks launched %v:\n%s", fix, out)
	}
	if out := statusRow(); !strings.Contains(out, "awaiting-approval") || strings.Contains(out, "pending") {
		t.Errorf("with no checks listed, status should read awaiting-approval and CI -:\n%s", out)
	}

	// Pending: the dashboard's first check records the change, and shows
	// the checks.
	h.serve(c, checks("pending", "pending", ""))
	_, screen := h.startDashboard(220)
	screen("CI …")
	h.tmux("kill-session", "-t", "dash")
	if out := statusRow(); !strings.Contains(out, "awaiting-approval") || !strings.Contains(out, "pending") {
		t.Errorf("status should read awaiting-approval with CI pending:\n%s", out)
	}

	// A run works on the change: its failed head gets no fix run.
	h.resetWorker()
	busy, _ := h.launchRun("--change", c.id, "--", "more work")
	h.WaitForClaudeStart(30 * time.Second)
	failed := checks("failed", "failed", "go test ./web/... failed:\n  TestServe")
	h.serve(c, failed)
	if out, fix := h.failedChecks(); fix != nil || !strings.Contains(out, "a run is working on the change") {
		t.Fatalf("with run %s working on the change, failed-checks launched %v:\n%s", busy.ID, fix, out)
	}
	h.finishRun(busy.ID)

	// The dashboard reports the failure, and dispatches one fix run.
	h.resetWorker()
	h.serve(c, failed)
	known := h.RunIDs()
	_, screen = h.startDashboard(220)
	screen("CI ✗", "valley-request")
	var fix *run.State
	for deadline := time.Now().Add(60 * time.Second); fix == nil && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		fix = h.newRun(known)
	}
	h.tmux("kill-session", "-t", "dash")
	if fix == nil {
		t.Fatal("the dashboard dispatched no fix run")
	}
	if fix.ChangeID != c.id || fix.Branch != c.branch || fix.DispatchReason != event.DispatchChecksFailed {
		t.Errorf("the fix run is on change %s, branch %s, for %q", fix.ChangeID, fix.Branch, fix.DispatchReason)
	}
	h.WaitForClaudeStart(30 * time.Second)
	brief := h.ClaudeStdin()
	for _, want := range []string{"valley-request", "TestServe", "valley telemetry --json read --source worker --logs",
		"--since", c.id, "valley checks --names origin/main HEAD", "--amend"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief should name %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "integrator-unit") {
		t.Errorf("the brief names a check that passed:\n%s", brief)
	}
	evts := checksEvents()
	if len(evts) != 1 || evts[0].Type != event.ChangeChecksFailed {
		t.Fatalf("checks events %v, want one change:checks-failed", evts)
	}
	if d := evts[0].Data; d["head"] != c.v1 || d["repo"] != "widget" || d["version"] != float64(1) ||
		!slices.Equal(d["failed"].([]any), []any{"valley-request"}) {
		t.Errorf("change:checks-failed carries %v", d)
	}
	h.finishRun(fix.ID)

	// The same failed head again: no second run, and no second event.
	h.serve(c, failed)
	if out, again := h.failedChecks(); again != nil || !strings.Contains(out, "fix run "+fix.ID+" dispatched (attempt 1 of 3)") {
		t.Fatalf("a second pass on the same failed head launched %v:\n%s", again, out)
	}
	if out := statusRow(); !strings.Contains(out, "failing") || !strings.Contains(out, "nxvp v1: valley-request") {
		t.Errorf("status should read CI failing and list the failed check:\n%s", out)
	}

	// Passed: reported once, and nothing dispatched.
	h.serve(c, checks("passed", "passed", ""))
	known = h.RunIDs()
	for i := 0; i < 2; i++ {
		_, screen = h.startDashboard(220)
		screen("CI ✓")
		h.tmux("kill-session", "-t", "dash")
	}
	if fix := h.newRun(known); fix != nil {
		t.Errorf("passed checks launched %s", fix.ID)
	}
	if evts := checksEvents(); len(evts) != 2 || evts[1].Type != event.ChangeChecksPassed {
		t.Errorf("checks events %v, want change:checks-failed then change:checks-passed", evts)
	}
	res := h.RunKlaus("notifications", "--all")
	if !strings.Contains(res.Stdout, "CI failed: widget nxvp v1: valley-request") || !strings.Contains(res.Stdout, "CI passed: widget nxvp v1") {
		t.Errorf("notifications should list the checks:\n%s", res.Stdout)
	}
}

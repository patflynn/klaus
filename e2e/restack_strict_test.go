//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/restack"
	"github.com/patflynn/klaus/internal/valley"
)

// restackListing is a valley-changes/v1 listing of widget with one change,
// id (handle nxvp), owned by owner and in state at v1, head on branch. Its
// latest restack report, commit report, says rebasing it onto tip
// conflicted in its own commits, and is current while the change is ready.
func restackListing(id, branch, head, state, owner, record, report, tip string) map[string]any {
	sha := func(oid string) string { return "git-sha1:" + oid }
	ch := map[string]any{
		"change_id": id, "handle": "nxvp", "target": "main", "owner": owner, "state": state,
		"record": record, "seq": 1, "active": 1, "includes": []string{},
		"versions": []any{map[string]any{"n": 1, "head": head, "title": "Add a greeting", "branch": branch,
			"uploader": "klaus-env", "landing": "not-landed", "landed_commit": nil, "request": nil, "outcome": nil}},
		"restack": map[string]any{"ref": restack.ReportRef("main", id), "commit": report, "current": state == valley.StateReady,
			"report": map[string]any{
				"schema": valley.RestackSchema, "change_id": id, "target": "main", "target_tip": sha(tip),
				"operation": "e2e", "result": valley.RestackConflict,
				"layers": []any{map[string]any{"change_id": id, "record": sha(record), "version": 1, "head": sha(head),
					"branch": branch, "branch_tip": sha(head), "published": nil}},
				"failing_layer": id, "conflict_paths": []string{"greeting.txt"}, "blocking_requests": []any{}, "refusal": nil,
			}},
	}
	return map[string]any{"schema": "valley-changes/v1", "host": "classic-laddie", "repos": []any{map[string]any{
		"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil, "changes": []any{ch}, "branches": []any{},
	}}}
}

// writeE2EJSON writes v as JSON to name in the e2e directory, where the
// stubs read their answers.
func (h *Harness) writeE2EJSON(name string, v any) {
	h.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.E2EDir, name), data, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// originCommit makes a commit on origin with an empty tree and returns it,
// for a ref the valley keeps there.
func (h *Harness) originCommit(msg string) string {
	h.t.Helper()
	return h.GitOutput(h.OriginDir, "commit-tree", h.gitStdin(h.OriginDir, "", "mktree"), "-m", msg)
}

// strictChange is a change klaus-env owns that a finished run pushed as v1
// on branch, with main moved on to tip since, and the change's record and
// a restack report on origin. The stub valley offers unready.
type strictChange struct{ id, branch, v1, record, report, tip string }

func (h *Harness) strictChange() strictChange {
	h.t.Helper()
	h.installValleyStub()
	h.joinValley()
	if err := os.WriteFile(filepath.Join(h.E2EDir, "valley.has-unready"), nil, 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.git(h.RepoDir, "config", "valley.principal", "klaus-env")

	first, _ := h.launchRun("add a greeting")
	h.WaitForClaudeStart(30 * time.Second)
	v1 := h.pushWork(first, "greeting.txt", "Add a greeting")
	first = h.finishRun(first.ID)
	c := strictChange{id: first.ChangeID, branch: first.Branch, v1: v1}
	c.tip = h.commitToMain("greeting.txt", "Greet from main\n")
	c.record, c.report = h.originCommit("record"), h.originCommit("report")
	h.git(h.OriginDir, "update-ref", restack.RecordRef("main", c.id), c.record)
	h.git(h.OriginDir, "update-ref", restack.ReportRef("main", c.id), c.report)
	return c
}

// listing is the change's listing in state, owned by klaus-env at v1.
func (c strictChange) listing(state string) map[string]any {
	return restackListing(c.id, c.branch, c.v1, state, "klaus-env", c.record, c.report, c.tip)
}

// launchArgs launch a run on the change as a restack pass does: under
// runID, expecting klaus-env's draft at v1 on its branch.
func (c strictChange) launchArgs(runID string) []string {
	return []string{"launch", "--change", c.id, "--run-id", runID, "--expect-owner", "klaus-env",
		"--expect-branch", c.branch, "--expect-head", c.v1, "--expect-draft", "--", "resolve the conflict"}
}

// livePanesNaming returns the live panes on the harness's tmux server
// whose command names s.
func (h *Harness) livePanesNaming(s string) []string {
	h.t.Helper()
	var live []string
	for _, line := range strings.Split(h.tmux("list-panes", "-a", "-F", "#{pane_dead} #{pane_id} #{pane_start_command}"), "\n") {
		if strings.HasPrefix(line, "0 ") && strings.Contains(line, s) {
			live = append(live, line)
		}
	}
	return live
}

// TestRestackConflictLaunchesStrictly runs against the stub valley, so it
// runs where no valley is installed. A run pushes v1 of a change klaus-env
// owns. Launch with the dispatcher's expectations refuses, with status 3,
// before it creates a worktree or returns the change to draft, a change
// that was readied, taken over or advanced since the dispatcher checked it.
// A restack pass then dispatches exactly one run, under the run ID its
// ledger recorded before launching, and the dashboard shows it without
// asking the valley more than its check does. And when the change is readied again
// between the pass's unready and the launch, no worker starts and the
// attempt is given back.
func TestRestackConflictLaunchesStrictly(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	c := h.strictChange()
	id, branch, v1, record, report, tip := c.id, c.branch, c.v1, c.record, c.report, c.tip

	for i, tc := range []struct {
		name, state, owner, head, want string
	}{
		{"readied since", valley.StateReady, "klaus-env", v1, "change nxvp is ready, not a draft"},
		{"another owner's", valley.StateDraft, "someone-else", v1, `change nxvp's owner is "someone-else", not "klaus-env"`},
		{"advanced since", valley.StateDraft, "klaus-env", strings.Repeat("9", 40), "change nxvp's head is 9999999"},
	} {
		h.writeValleyChanges(restackListing(id, branch, tc.head, tc.state, tc.owner, record, report, tip))
		runID := fmt.Sprintf("20261027-1200-%08x", i+1)
		unreadies, runs := len(h.valleyCalls("unready")), len(h.RunIDs())
		res := h.RunKlaus("launch", "--change", id, "--run-id", runID, "--expect-owner", "klaus-env",
			"--expect-branch", branch, "--expect-head", v1, "--expect-draft", "--", "resolve the conflict")
		if res.ExitCode != 3 || !strings.Contains(res.Stderr, tc.want) {
			t.Errorf("%s: launch exited %d, want 3 saying %q:\n%s", tc.name, res.ExitCode, tc.want, res.Stderr)
		}
		if n := len(h.RunIDs()); n != runs {
			t.Errorf("%s: launch recorded a run", tc.name)
		}
		if wt := h.GitOutput(h.RepoDir, "worktree", "list"); strings.Contains(wt, runID) {
			t.Errorf("%s: launch left a worktree:\n%s", tc.name, wt)
		}
		if n := len(h.valleyCalls("unready")); n != unreadies {
			t.Errorf("%s: launch returned the change to draft before refusing", tc.name)
		}
	}
	if res := h.RunKlaus("launch", "--change", id, "--expect-owner", "nobody", "--", "x"); res.ExitCode != 1 {
		t.Errorf("without --run-id a refusal exits 1, exited %d", res.ExitCode)
	}

	// One pass, one run, under the ID the ledger recorded first.
	ready := restackListing(id, branch, v1, valley.StateReady, "klaus-env", record, report, tip)
	draft := restackListing(id, branch, v1, valley.StateDraft, "klaus-env", record, report, tip)
	h.writeValleyChanges(ready)
	h.writeE2EJSON("valley.changes.unready.json", draft)
	h.resetWorker()
	out, resolver := h.restackPass()
	if resolver == nil {
		t.Fatalf("the pass launched nothing:\n%s", out)
	}
	e := h.restackEntry("widget", id)
	if e.Last().RunID != resolver.ID || e.Last().Pending || e.Attempts != 1 {
		t.Errorf("the ledger should hold the launched run %s, settled, as attempt 1: %+v", resolver.ID, e.Last())
	}
	if resolver.ChangeID != id || resolver.Branch != branch || resolver.DispatchReason != event.DispatchRestackConflict {
		t.Errorf("the run is on change %s, branch %s, for %q", resolver.ChangeID, resolver.Branch, resolver.DispatchReason)
	}
	h.WaitForClaudeStart(30 * time.Second)
	if brief := h.ClaudeStdin(); !strings.Contains(brief, v1) || !strings.Contains(brief, tip) || strings.Contains(brief, "greeting.txt") {
		t.Errorf("the brief should name the head and main's tip, and not the report's paths:\n%s", brief)
	}
	if out, again := h.restackPass(); again != nil {
		t.Fatalf("a second pass launched %s:\n%s", again.ID, out)
	}

	// The real dashboard shows the dispatch on the change's row, and its
	// restack pass reads the integration check's listing: the check asks
	// the valley once, and the pass asks nothing more.
	before := len(h.valleyCalls("changes"))
	_, screen := h.startDashboard(220)
	screen("restack conflict", "resolution run "+resolver.ID+" dispatched (attempt 1 of 3)")
	if calls := h.valleyCalls("changes")[before:]; len(calls) != 1 || !strings.HasSuffix(calls[0], ": changes --all --json") {
		t.Errorf("valley changes calls from the dashboard = %q, want one listing for every repo", calls)
	}
	h.tmux("kill-session", "-t", "dash")
	h.finishRun(resolver.ID)

	// main moves on and the restacker reports again; the change is
	// readied again after the pass's unready and before launch reads it.
	tip = h.commitToMain("farewell.txt", "Farewell from main\n")
	report = h.originCommit("report 2")
	h.git(h.OriginDir, "update-ref", restack.ReportRef("main", id), report)
	h.writeValleyChanges(restackListing(id, branch, v1, valley.StateReady, "klaus-env", record, report, tip))
	h.writeE2EJSON("valley.changes.unready.1.json", restackListing(id, branch, v1, valley.StateDraft, "klaus-env", record, report, tip))
	h.writeE2EJSON("valley.changes.unready.json", restackListing(id, branch, v1, valley.StateReady, "klaus-env", record, report, tip))
	h.ageLedger()
	h.resetWorker()
	out, raced := h.restackPass()
	if raced != nil || !strings.Contains(out, "not dispatched: launch failed, and the change is left a draft") ||
		!strings.Contains(out, "is ready, not a draft") {
		t.Fatalf("a change readied before launch read it got run %v:\n%s", raced, out)
	}
	if e := h.restackEntry("widget", id); e.Attempts != 1 || e.Last().RunID != "" || e.Last().Pending {
		t.Errorf("a launch that started no worker should give its attempt back: attempts %d, %+v", e.Attempts, e.Last())
	}
	if _, err := os.Stat(filepath.Join(h.E2EDir, "claude.started")); err == nil {
		t.Error("a worker started")
	}
}

// TestStrictLaunchReadsOriginUnderTheLock: origin's branch moves on, with
// no new version in the valley, while a strict launch waits for the
// continuation lock. Launch reads origin under the lock, not as its fetch
// before it found it, and refuses with status 3 before it creates a
// worktree.
func TestStrictLaunchReadsOriginUnderTheLock(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	c := h.strictChange()
	h.writeValleyChanges(c.listing(valley.StateDraft))

	lock, err := os.OpenFile(filepath.Join(h.RepoDir, ".git", "klaus-continue.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	const runID = "20261027-1200-0000000a"
	p := h.startKlaus(nil, c.launchArgs(runID)...)
	p.waitFor(func() bool {
		_, stderr := p.output()
		return strings.Contains(stderr, "waiting for another launch")
	}, "to wait for the continuation lock")

	tree := h.GitOutput(h.OriginDir, "rev-parse", c.v1+"^{tree}")
	moved := h.GitOutput(h.OriginDir, "commit-tree", tree, "-p", c.v1, "-m", "pushed elsewhere")
	h.git(h.OriginDir, "update-ref", "refs/heads/"+c.branch, moved)
	lock.Close() // drops the flock

	res := p.wait()
	want := fmt.Sprintf("origin's %s is at %s, not change nxvp's expected head %s", c.branch, moved[:7], c.v1[:7])
	if res.ExitCode != 3 || !strings.Contains(res.Stderr, want) {
		t.Fatalf("launch exited %d, want 3 saying %q:\n%s", res.ExitCode, want, res.Stderr)
	}
	if wt := h.GitOutput(h.RepoDir, "worktree", "list"); strings.Contains(wt, runID) {
		t.Errorf("launch left a worktree:\n%s", wt)
	}
	if n := len(h.valleyCalls("unready")); n != 0 {
		t.Errorf("launch returned the change to draft before refusing")
	}
}

// TestStrictLaunchRefusesAChangeReadiedDuringSetup: a strict launch passes
// its checks and returns the change to draft, and the change is readied
// while launch sets the worker up, after the point it used to check last.
// Launch asks again as the worker starts, and starts none.
func TestStrictLaunchRefusesAChangeReadiedDuringSetup(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	c := h.strictChange()
	h.writeValleyChanges(c.listing(valley.StateDraft))
	h.writeE2EJSON("valley.changes.unready.json", c.listing(valley.StateDraft))
	h.resetWorker()

	// The last setup step before the worker starts snapshots the clone's
	// shared git files, its config among them.
	b := h.gitBarrier()
	const runID = "20261027-1200-0000000b"
	p := h.startKlaus(b.pause("config --file * --list -z", 1, false), c.launchArgs(runID)...)
	b.waitPaused(p)
	if n := len(h.valleyCalls("unready")); n != 1 {
		t.Fatalf("held before launch returned the change to draft (%d unready calls)", n)
	}
	h.writeE2EJSON("valley.changes.unready.json", c.listing(valley.StateReady))
	b.release()

	res := p.wait()
	if res.ExitCode != 3 || !strings.Contains(res.Stderr, "change nxvp is ready, not a draft") {
		t.Fatalf("launch exited %d, want 3 refusing the readied change:\n%s", res.ExitCode, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(h.E2EDir, "claude.started")); err == nil {
		t.Error("a worker started")
	}
	if slices.Contains(h.RunIDs(), runID) {
		t.Error("launch recorded the run")
	}
	if wt := h.GitOutput(h.RepoDir, "worktree", "list"); strings.Contains(wt, runID) {
		t.Errorf("launch left a worktree:\n%s", wt)
	}
}

// TestRestackLaunchWhoseStateSaveFails: a restack pass launches a run whose
// worker starts and whose state launch can't save. Launch stops the worker
// and exits 1, as a worker started; the pass keeps the attempt and, once
// it proves no worker of the run is left, settles the launch. While a live
// pane still names the run, the change stays busy whatever its inputs; once
// none does, the next inputs dispatch.
func TestRestackLaunchWhoseStateSaveFails(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	c := h.strictChange()
	h.writeValleyChanges(c.listing(valley.StateReady))
	h.writeE2EJSON("valley.changes.unready.json", c.listing(valley.StateDraft))
	h.resetWorker()

	if err := os.Chmod(h.RunsDir(), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.RunsDir(), 0o755) })
	out, launched := h.restackPass()
	if err := os.Chmod(h.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	e := h.restackEntry("widget", c.id)
	runID := e.Last().RunID
	if launched != nil || runID == "" || e.Attempts != 1 || e.Last().Pending ||
		!strings.Contains(e.Last().Refused, "no worker of run "+runID+" runs") || !strings.Contains(e.Last().Refused, "saving state") {
		t.Fatalf("the pass should keep the attempt and settle a launch whose worker launch stopped: %+v\n%s", e.Last(), out)
	}
	if live := h.livePanesNaming(runID); len(live) != 0 {
		t.Errorf("a worker no run records is left running: %v", live)
	}
	if wt := h.GitOutput(h.RepoDir, "worktree", "list"); strings.Contains(wt, runID) {
		t.Errorf("launch left a worktree:\n%s", wt)
	}

	// Had its worker been left running, as a pass that lost sight of the
	// launch finds it, the change is busy: main moves on, and nothing
	// launches while a live pane names the run.
	h.editLedger(func(entries map[string]*restack.Entry) {
		a := entries[restack.Key{Repo: "widget", ChangeID: c.id}.String()].Last()
		a.Pending, a.Refused = true, ""
	})
	h.tmux("new-session", "-d", "-s", "holdout", "sleep 300 # "+runID)
	if live := h.livePanesNaming(runID); len(live) != 1 {
		t.Fatalf("the stand-in worker's pane isn't found by its command: %v", live)
	}
	c.tip = h.commitToMain("farewell.txt", "Farewell from main\n")
	c.report = h.originCommit("report 2")
	h.git(h.OriginDir, "update-ref", restack.ReportRef("main", c.id), c.report)
	h.writeValleyChanges(c.listing(valley.StateReady))
	h.writeE2EJSON("valley.changes.unready.json", c.listing(valley.StateDraft))
	os.Remove(filepath.Join(h.E2EDir, "valley.after-unready")) // the stub lists the ready change until the next unready
	h.ageLedger()
	unreadies := len(h.valleyCalls("unready"))
	out, launched = h.restackPass()
	if launched != nil || !strings.Contains(out, "launching resolution run "+runID) || len(h.valleyCalls("unready")) != unreadies {
		t.Fatalf("a change whose worker may run got another run %v:\n%s", launched, out)
	}

	h.tmux("kill-session", "-t", "holdout")
	h.resetWorker()
	out, launched = h.restackPass()
	if launched == nil {
		t.Fatalf("once no worker of the lost launch runs, the new inputs should dispatch:\n%s", out)
	}
	if e := h.restackEntry("widget", c.id); e.Attempts != 2 || e.Last().RunID != launched.ID {
		t.Errorf("attempts %d, last %+v; want the lost launch's attempt kept and the new run as attempt 2", e.Attempts, e.Last())
	}
	if n := len(h.eventsOfType(event.ChangeRestackStalled)); n != 2 {
		t.Errorf("%d stalled events, want one for each launch that came to nothing", n)
	}
}

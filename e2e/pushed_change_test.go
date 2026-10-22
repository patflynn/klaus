//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
)

// listing is a valley-changes/v1 listing of the sandbox repo, widget, with
// the given change records.
func listing(changes ...any) map[string]any {
	return map[string]any{
		"schema": "valley-changes/v1",
		"host":   "classic-laddie",
		"repos": []any{map[string]any{"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil,
			"branches": []any{}, "changes": changes}},
	}
}

// changeRecord is a valley change record with the given ID and handle, in
// state, whose one version is head on branch.
func changeRecord(id, handle, state, head, branch, title string) map[string]any {
	v := version(1, head, title, "not-landed")
	v["branch"] = branch
	return map[string]any{"change_id": id, "handle": handle, "target": "main", "owner": "klaus-env",
		"state": state, "record": "0000000000000000000000000000000000000000", "seq": 1, "active": 1,
		"versions": []any{v}, "includes": []string{}}
}

// TestFinalizeRecordsThePushedChange replays run 20261022-1036-d8deeb25: a
// --resume-from run continues another run's valley change on its branch,
// and its agent makes a new change instead, switching to a branch of its
// own from origin/main and committing with its own Change-Id. Finalize
// records the pushed change and branch in place of launch's, marks the
// pushed head ready as a version of that change, and says so in
// agent:change-recorded. klaus status and the dashboard then show the run
// under the change it pushed, apart from the one it was launched on.
func TestFinalizeRecordsThePushedChange(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.RegisterProject("acme/widget", h.RepoDir)
	h.installValleyStub()
	h.joinValley()
	if err := os.WriteFile(filepath.Join(h.E2EDir, "valley.has-unready"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")

	first, _ := h.launchRun("build approval keys")
	h.WaitForClaudeStart(30 * time.Second)
	v1 := h.pushWork(first, "keys.go", "Build approval keys")
	first = h.finishRun(first.ID)
	h.writeValleyChanges(listing(changeRecord(first.ChangeID, "qvzz", "ready", v1, first.Branch, "Build approval keys")))

	h.resetWorker()
	follow, _ := h.launchRun("--repo", "widget", "--resume-from", first.ID, "land the registry fix on its own")
	h.WaitForClaudeStart(30 * time.Second)
	if follow.Branch != first.Branch || follow.ChangeID != first.ChangeID {
		t.Fatalf("follow-up on branch %q, change %q; want it to continue %q, %q", follow.Branch, follow.ChangeID, first.Branch, first.ChangeID)
	}

	// The agent makes a new change from origin/main instead.
	worktree := follow.Worktree
	const branch, subject = "registry-reads-its-commit", "Read the registry through its commit"
	newID := "vpkz" + strings.Repeat("pk", 14)
	h.git(worktree, "switch", "-q", "-c", branch, "origin/main")
	if err := os.WriteFile(filepath.Join(worktree, "registry.go"), []byte("package registry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.git(worktree, "add", "-A")
	h.git(worktree, "commit", "-qm", subject+"\n\nChange-Id: "+newID)
	h.git(worktree, "push", "-q", "-u", "origin", "HEAD")
	head := h.GitOutput(worktree, "rev-parse", "HEAD")
	if got := h.GitOutput(worktree, "log", "-1", "--format=%(trailers:key=Change-Id,valueonly)"); strings.TrimSpace(got) != newID {
		t.Fatalf("the commit's Change-Id = %q, want the agent's own %s", got, newID)
	}
	follow = h.finishRun(follow.ID)

	if follow.ChangeID != newID || follow.Branch != branch {
		t.Errorf("run recorded change %q on branch %q; want the pushed %s on %s", follow.ChangeID, follow.Branch, newID, branch)
	}
	if follow.PushedBranch == nil || *follow.PushedBranch != branch || follow.PushedSHA == nil || *follow.PushedSHA != head {
		t.Errorf("pushed %v at %v; want %s at %s", follow.PushedBranch, follow.PushedSHA, branch, head)
	}
	if follow.PushedSubject != subject {
		t.Errorf("pushed subject = %q, want %q", follow.PushedSubject, subject)
	}
	// Launch's draft of the change it continued stays on record, as that
	// change's.
	if follow.Unready == nil || follow.Unready.ChangeID != first.ChangeID || follow.Unready.Handle != "nxvp" {
		t.Errorf("unready = %+v, want launch's draft of %s", follow.Unready, first.ChangeID)
	}
	ready := fmt.Sprintf("%s: ready --change %s --head %s %s", worktree, newID, head, branch)
	if calls := h.valleyCalls("ready"); len(calls) != 2 || calls[1] != ready {
		t.Errorf("valley ready calls = %q, want the second to be %q", calls, ready)
	}
	var types []string
	var recorded map[string]any
	for _, e := range h.runEvents(follow.ID) {
		types = append(types, e.Type)
		if e.Type == event.AgentChangeRecorded {
			recorded = e.Data
		}
	}
	want := map[string]any{"id": follow.ID, "change_id": newID, "branch": branch, "head": head,
		"launch_change_id": first.ChangeID, "launch_branch": first.Branch,
		"repo_key": "valley:widget", "key": "valley:widget@" + newID, "dispatch_reason": "resume", "head_sha": head}
	if fmt.Sprint(recorded) != fmt.Sprint(want) {
		t.Errorf("agent:change-recorded data = %v, want %v", recorded, want)
	}
	if i := strings.Index(strings.Join(types, ","), event.AgentChangeRecorded+","+event.AgentCompleted); i < 0 {
		t.Errorf("events = %v, want agent:change-recorded just before agent:completed", types)
	}

	// The valley lists both changes: the one the run was launched on,
	// withdrawn, and the one it pushed, ready.
	h.writeValleyChanges(listing(
		changeRecord(first.ChangeID, "qvzz", "withdrawn", v1, first.Branch, "Build approval keys"),
		changeRecord(newID, "vpkz", "ready", head, branch, subject)))
	res := h.RunKlaus("status")
	if res.ExitCode != 0 {
		t.Fatalf("status exited %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	for id, want := range map[string][]string{
		follow.ID: {"awaiting-approval", " vpkz ", subject},
		first.ID:  {"withdrawn", " qvzz "},
	} {
		line := lineContaining(res.Stdout, id)
		for _, w := range want {
			if !strings.Contains(line, w) {
				t.Errorf("status line for %s lacks %q:\n%s", id, w, res.Stdout)
			}
		}
	}

	_, screen := h.startDashboard(160)
	row := rowOf(t, screen("vpkz", subject), "vpkz")
	for _, w := range []string{"READY", subject} {
		if !strings.Contains(row, w) {
			t.Errorf("dashboard row for vpkz lacks %q: %q", w, row)
		}
	}
}

// TestTrackValleyChange tracks a valley change no run pushed, by handle,
// and finds it shown like a tracked PR: klaus status lists it where the
// valley's listing says it stands, under its handle and title, and so does
// the dashboard. The other forms of ref name the same change, so it isn't
// added twice; a change the valley doesn't list adds nothing. Untrack takes
// the same refs and removes it.
func TestTrackValleyChange(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.RegisterProject("acme/widget", h.RepoDir)
	h.installValleyStub()
	const title = "Read the registry through its commit"
	id := "vpkz" + strings.Repeat("pk", 14)
	head := h.GitOutput(h.RepoDir, "rev-parse", "HEAD")
	h.writeValleyChanges(listing(changeRecord(id, "vpkz", "ready", head, "registry-reads-its-commit", title)))

	res := h.RunKlaus("track", "--repo", "widget", "vpkz")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "Tracking change vpkz (widget) — "+title) {
		t.Fatalf("track exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if calls := h.valleyCalls("changes"); len(calls) != 1 || calls[0] != h.RepoDir+": changes --json" {
		t.Errorf("valley changes calls = %q, want one in the project's checkout", calls)
	}
	res = h.RunKlaus("track", "widget:vpkz", "--repo", "widget", id, "zzzz")
	if want := "Already tracking change vpkz (widget)\nAlready tracking change vpkz (widget)\n"; res.Stdout != want {
		t.Errorf("tracking the change again by its other refs printed %q, want %q", res.Stdout, want)
	}
	if want := "warning: zzzz: the valley lists no change zzzz in widget"; !strings.Contains(res.Stderr, want) {
		t.Errorf("track of an unlisted change should warn %q:\n%s", want, res.Stderr)
	}
	ids := h.RunIDs()
	if len(ids) != 1 {
		t.Fatalf("track recorded %d states, want 1", len(ids))
	}
	st, err := h.ReadState(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if st.Type != "track" || st.ChangeID != id || st.Valley == nil || st.Valley.Repo != "widget" || st.Valley.Handle != "vpkz" || st.HeadSHA != head {
		t.Errorf("track record = %+v, valley %+v", st, st.Valley)
	}

	status := func(want ...string) {
		t.Helper()
		res := h.RunKlaus("status")
		line := lineContaining(res.Stdout, st.ID)
		for _, w := range want {
			if !strings.Contains(line, w) {
				t.Errorf("status line for the tracked change lacks %q:\n%s%s", w, res.Stdout, res.Stderr)
			}
		}
	}
	status("awaiting-approval", " vpkz ", title)

	_, screen := h.startDashboard(160)
	if row := rowOf(t, screen("vpkz", title), "vpkz"); !strings.Contains(row, "READY") {
		t.Errorf("dashboard row for the tracked change = %q, want READY", row)
	}

	// The valley says it landed; then it can't be asked.
	landed := changeRecord(id, "vpkz", "ready", head, "registry-reads-its-commit", title)
	landed["versions"].([]any)[0].(map[string]any)["landing"] = "landed"
	h.writeValleyChanges(listing(landed))
	status("integrated", " vpkz ")
	if err := os.Remove(filepath.Join(h.E2EDir, "valley.changes.json")); err != nil {
		t.Fatal(err)
	}
	status("tracking", " vpkz ")

	res = h.RunKlaus("untrack", "widget:vpkz")
	if !strings.Contains(res.Stdout, "Stopped tracking change vpkz (widget)") {
		t.Errorf("untrack output:\n%s%s", res.Stdout, res.Stderr)
	}
	if ids := h.RunIDs(); len(ids) != 0 {
		t.Errorf("untrack left %d states", len(ids))
	}
	if res := h.RunKlaus("untrack", "vpkz"); !strings.Contains(res.Stderr, "warning: no tracked change vpkz found") {
		t.Errorf("untracking it again should warn:\n%s%s", res.Stdout, res.Stderr)
	}
}

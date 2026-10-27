//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
)

// installValleyStub writes a fake valley with the verbs klaus uses. It logs
// each call ("<cwd>: <args>") to valley.argv, answers ready with
// "widget nxvp v1 ready" or, when valley.refuse exists, refuses with its
// contents, and answers changes with valley.changes.json or, when that is
// absent, fails as an unreachable valley does. Its help lists unready only
// when valley.has-unready exists; unready answers "widget nxvp v1 draft" or,
// when valley.unready-refuse exists, refuses with its contents, and creates
// valley.unready-late if the fake agent had already started. After an
// unready, the n-th changes answers with valley.changes.unready.<n>.json, or
// valley.changes.unready.json, when one exists, so a test can say what the
// valley lists once a change is a draft. Its knot verbs serve the knots
// writeKnot wrote (see there).
func (h *Harness) installValleyStub() {
	h.t.Helper()
	h.WriteStub("valley", fmt.Sprintf(`#!/usr/bin/env bash
dir=%q
{ printf '%%s:' "$PWD"; for a in "$@"; do printf ' %%s' "$a"; done; printf '\n'; } >> "$dir/valley.argv"
case "$1" in
help)
  printf 'usage: valley <verb> [args]\n\n  ready --change <id> --head <sha> <branch>\n  changes [--all] [--json]\n  approve <change>\n'
  if [ -f "$dir/valley.has-unready" ]; then printf '  unready <change>\n'; fi
  printf '  knot <verb> [args]\n'
  ;;
knot)
  id=$3
  case "$2" in
  show)
    if [ -f "$dir/knot.$id.json" ]; then cat "$dir/knot.$id.json"; exit 0; fi
    echo "valley: the graph of widget holds no knot $id" >&2
    exit 1
    ;;
  claim)
    if [ -f "$dir/claude.started" ]; then : > "$dir/knot.claim-late"; fi
    if [ -f "$dir/knot.claim-refuse" ]; then cat "$dir/knot.claim-refuse" >&2; exit 3; fi
    echo "knot claim $id: $5 (0000000)"
    ;;
  link)
    if [ -f "$dir/knot.link-refuse" ]; then cat "$dir/knot.link-refuse" >&2; exit 1; fi
    echo "knot link $id (0000000)"
    ;;
  set)
    if [ -f "$dir/knot.$id.set-refuse" ]; then cat "$dir/knot.$id.set-refuse" >&2; exit 3; fi
    echo "knot set $id (0000000)"
    ;;
  comment)
    { printf '== %%s\n' "$id"; cat; } >> "$dir/knot.comments"
    echo "knot comment $id (0000000)"
    ;;
  *)
    echo "knot $2 $id (0000000)"
    ;;
  esac
  ;;
ready)
  if [ -f "$dir/valley.refuse" ]; then cat "$dir/valley.refuse" >&2; exit 1; fi
  echo "widget nxvp v1 ready"
  ;;
unready)
  if [ -f "$dir/claude.started" ]; then : > "$dir/valley.unready-late"; fi
  if [ -f "$dir/valley.unready-refuse" ]; then cat "$dir/valley.unready-refuse" >&2; exit 1; fi
  : > "$dir/valley.after-unready"
  echo "widget nxvp v1 draft"
  ;;
changes)
  if [ -f "$dir/valley.after-unready" ]; then
    echo >> "$dir/valley.after-unready"
    n=$(wc -l < "$dir/valley.after-unready" | tr -d ' ')
    for f in "$dir/valley.changes.unready.$n.json" "$dir/valley.changes.unready.json"; do
      if [ -f "$f" ]; then cat "$f"; exit 0; fi
    done
  fi
  if [ -f "$dir/valley.changes.json" ]; then cat "$dir/valley.changes.json"; exit 0; fi
  echo "valley: cannot reach classic-laddie" >&2
  exit 1
  ;;
*)
  echo "valley: unknown verb $1" >&2
  exit 2
  ;;
esac
`, h.E2EDir))
}

// joinValley commits the valley's policy to the sandbox repo's main and
// pushes it, so the repo delivers to a valley.
func (h *Harness) joinValley() {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Join(h.RepoDir, "policy"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.RepoDir, "policy", "valley"), []byte("widget\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.git(h.RepoDir, "add", "policy")
	h.git(h.RepoDir, "commit", "-qm", "join the valley")
	h.git(h.RepoDir, "push", "-q", "origin", "main")
}

// valleyCalls returns the logged valley calls whose arguments start with
// verb.
func (h *Harness) valleyCalls(verb string) []string {
	data, _ := os.ReadFile(filepath.Join(h.E2EDir, "valley.argv"))
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if _, args, ok := strings.Cut(line, ": "); ok && strings.HasPrefix(args, verb) {
			calls = append(calls, line)
		}
	}
	return calls
}

// writeValleyChanges sets what the fake valley's changes verb prints.
func (h *Harness) writeValleyChanges(v any) {
	h.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.E2EDir, "valley.changes.json"), data, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// runEvents returns the session's events for one run, in order.
func (h *Harness) runEvents(runID string) []event.Event {
	h.t.Helper()
	f, err := os.Open(filepath.Join(h.Home, ".klaus", "sessions", h.SessionID, "events.jsonl"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	var evts []event.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e event.Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.RunID == runID {
			evts = append(evts, e)
		}
	}
	return evts
}

// TestFinalizeMarksValleyChangeReady launches an agent that commits and
// pushes its branch, and lets the real finalize run against a fake valley:
// in a repo that carries the valley's policy it marks the head ready and
// records the version; in a plain direct-push repo it never runs valley; and
// a refusal is recorded without failing finalize. The agent's log names a
// GitHub PR, as a test fixture would, and none of these runs takes it for
// theirs: their repos deliver by push.
func TestFinalizeMarksValleyChangeReady(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy bool
		refuse string
	}{
		{name: "valley repo", policy: true},
		{name: "direct-push repo without the policy"},
		{name: "valley refuses", policy: true, refuse: "valley: refused: no principal for this host\n(set valley.principal)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.installValleyStub()
			if tc.policy {
				h.joinValley()
			}
			if tc.refuse != "" {
				if err := os.WriteFile(filepath.Join(h.E2EDir, "valley.refuse"), []byte(tc.refuse), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			const fixturePR = "https://github.com/acme/widget/pull/7"
			h.WriteDefaultClaudeOutput(`{"type":"user","message":{"content":[{"type":"tool_result","content":"--- FAIL: want ` + fixturePR + `"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"The knot links ` + fixturePR + `; fixing the test."}]}}
{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"11111111-2222-4333-8444-555555555555"}` + "\n")

			res := h.RunKlaus("launch", "add a health check endpoint")
			if res.ExitCode != 0 {
				t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
			}
			id := h.RunIDs()[0]
			h.WaitForClaudeStart(30 * time.Second)
			st, err := h.ReadState(id)
			if err != nil {
				t.Fatal(err)
			}
			worktree := st.Worktree
			if err := os.WriteFile(filepath.Join(worktree, "health.go"), []byte("package health\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			h.git(worktree, "add", "-A")
			h.git(worktree, "commit", "-qm", "Add a health check")
			h.git(worktree, "push", "-q", "origin", st.Branch)
			head := h.GitOutput(worktree, "rev-parse", "HEAD")

			h.ReleaseClaude()
			st = h.WaitForState(id, func(s *run.State) bool {
				return s.DurationMS != nil && s.TmuxPane == nil && s.Worktree == ""
			}, 60*time.Second)
			if st.Delivery != run.DeliveryPush {
				t.Errorf("delivery = %q, want %q", st.Delivery, run.DeliveryPush)
			}
			if st.PRURL != nil {
				t.Errorf("PR URL = %q, want none from a run that delivers by push", *st.PRURL)
			}
			if st.PushedBranch == nil || *st.PushedBranch != st.Branch {
				t.Errorf("pushed branch = %v, want %s", st.PushedBranch, st.Branch)
			}
			if st.PushedSHA == nil || *st.PushedSHA != head {
				t.Fatalf("pushed SHA = %v, want %s", st.PushedSHA, head)
			}
			// The head's subject titles the change's row, valley or not.
			if st.PushedSubject != "Add a health check" {
				t.Errorf("pushed subject = %q, want the head's subject", st.PushedSubject)
			}

			var types []string
			var ready map[string]any
			for _, e := range h.runEvents(id) {
				types = append(types, e.Type)
				if e.Type == event.AgentChangeReady {
					ready = e.Data
				}
			}
			calls := h.valleyCalls("ready")

			switch {
			case !tc.policy:
				if len(calls) != 0 || st.Valley != nil || ready != nil {
					t.Errorf("a repo without the policy must not run valley ready: calls %v, valley %+v, event %v", calls, st.Valley, ready)
				}
			case tc.refuse != "":
				if st.Valley == nil || st.Valley.Handle != "" ||
					st.Valley.Error != "valley ready: valley: refused: no principal for this host; (set valley.principal)" {
					t.Errorf("refusal recorded as %+v", st.Valley)
				}
				if ready != nil {
					t.Errorf("a refused change must not emit agent:change-ready: %v", ready)
				}
			default:
				want := fmt.Sprintf("%s: ready --change %s --head %s %s", worktree, st.ChangeID, head, st.Branch)
				if len(calls) != 1 || calls[0] != want {
					t.Errorf("valley ready calls = %q, want [%q]", calls, want)
				}
				if st.Valley == nil || *st.Valley != (run.Valley{Repo: "widget", Handle: "nxvp", Version: 1, Title: "Add a health check"}) {
					t.Errorf("recorded valley = %+v", st.Valley)
				}
				// Its own fields, and the keys every run event carries: the
				// valley's name for the repo is the one it reported.
				wantData := map[string]any{"id": id, "repo": "widget", "change_id": st.ChangeID, "handle": "nxvp",
					"version": float64(1), "title": "Add a health check", "head": head,
					"repo_key": "valley:widget", "key": "valley:widget@" + st.ChangeID, "dispatch_reason": "launch", "head_sha": head}
				if fmt.Sprint(ready) != fmt.Sprint(wantData) {
					t.Errorf("agent:change-ready data = %v, want %v", ready, wantData)
				}
			}
			for _, typ := range types {
				if typ == event.AgentPRCreated {
					t.Errorf("a run that delivers by push must not emit %s: events %v", typ, types)
				}
			}
			// Finalize always completes, and the branch is reported pushed before
			// any valley call; the finalize phase ends after both.
			want := []string{event.AgentCompleted, event.AgentBranchPushed}
			if ready != nil {
				want = append(want, event.AgentChangeReady)
			}
			want = append(want, event.PhaseCompleted)
			if len(types) < len(want) || strings.Join(types[len(types)-len(want):], ",") != strings.Join(want, ",") {
				t.Errorf("events = %v, want them to end with %v", types, want)
			}
		})
	}
}

// valleyRun seeds a run that finalize marked ready on the valley as version
// version of a change with the given handle, pushed at sha from the sandbox
// repo.
func (h *Harness) valleyRun(id, prompt, handle, title string, version int, sha string, created time.Time) *run.State {
	h.t.Helper()
	clone, branch, cost := h.RepoDir, "agent/"+id, 1.0
	changeID := handle + strings.Repeat("k", 32-len(handle))
	st := &run.State{
		ID: id, Type: "launch", Prompt: prompt, TargetRepo: strPtrE2E("acme/widget"), CloneDir: &clone,
		Branch: branch, PushedBranch: &branch, PushedSHA: &sha, CostUSD: &cost, ChangeID: changeID,
		CreatedAt: created.Format(time.RFC3339),
		Valley:    &run.Valley{Repo: "widget", Handle: handle, Version: version, Title: title},
	}
	h.SeedState(st)
	return st
}

// version is a valley-changes/v1 version.
func version(n int, head, title, landing string) map[string]any {
	return map[string]any{"n": n, "head": head, "title": title, "branch": "agent/x", "uploader": "klaus-env",
		"landing": landing, "landed_commit": nil, "request": nil, "outcome": nil}
}

// change is a valley-changes/v1 change record.
func change(st *run.State, state string, active int, versions ...map[string]any) map[string]any {
	return map[string]any{"change_id": st.ChangeID, "handle": st.Valley.Handle, "target": "main", "owner": "klaus-env",
		"state": state, "record": "0000000000000000000000000000000000000000", "seq": 1, "active": active,
		"versions": versions, "includes": []string{}}
}

// startDashboard runs the real dashboard in its own tmux session and returns
// a function that waits until the screen shows every wanted string.
func (h *Harness) startDashboard(width int) (pane string, screen func(want ...string) string) {
	h.t.Helper()
	pane = strings.TrimSpace(h.tmux("new-session", "-d", "-s", "dash", "-x", fmt.Sprint(width), "-y", "16", "-c", h.RepoDir,
		"-P", "-F", "#{pane_id}", "env KLAUS_SESSION_ID="+h.SessionID+" klaus dashboard"))
	return pane, func(want ...string) string {
		h.t.Helper()
		var last string
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			last = h.tmux("capture-pane", "-p", "-t", pane)
			ok := true
			for _, w := range want {
				ok = ok && strings.Contains(last, w)
			}
			if ok {
				return last
			}
		}
		h.t.Fatalf("dashboard screen never showed %q; last screen:\n%s", want, last)
		return ""
	}
}

// rowOf returns the screen line for the row with the given handle.
func rowOf(t *testing.T, screen, handle string) string {
	t.Helper()
	for _, line := range strings.Split(screen, "\n") {
		if f := strings.Fields(strings.TrimPrefix(line, ">")); len(f) > 1 && f[1] == handle {
			return line
		}
	}
	t.Fatalf("no row for %s on screen:\n%s", handle, screen)
	return ""
}

// TestDashboardShowsValleyChanges runs the real dashboard and klaus status
// over runs a fake valley reports in every state: each valley row shows the
// repo, the handle and the title, under its label and in its group, and the
// footer offers the approve command bound to the head on screen. A change
// whose active version was marked ready elsewhere reads as that version,
// while klaus status shows the local run's own version superseded.
func TestDashboardShowsValleyChanges(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub()
	h.AmendRepoConfig(map[string]any{
		"dashboard": map[string]any{"approve_command": "cd ~/hack/{repo} && valley approve {handle} --head {head}"},
	})
	sha := func(c byte) string { return strings.Repeat(string(c), 40) }
	ago := func(m int) time.Time { return time.Now().Add(-time.Duration(m) * time.Minute) }

	ready := h.valleyRun("20261013-0900-0001", "prompt one", "nxvp", "Add a health check", 1, sha('1'), ago(5))
	uncertain := h.valleyRun("20261013-0900-0002", "prompt two", "uncr", "Tune the cache", 1, sha('2'), ago(10))
	landed := h.valleyRun("20261013-0900-0003", "prompt three", "lndd", "Fix the build", 1, sha('3'), ago(15))
	superseded := h.valleyRun("20261013-0900-0004", "prompt four", "sprs", "Rename the flag", 1, sha('4'), ago(20))
	withdrawn := h.valleyRun("20261013-0900-0005", "prompt five", "wdrn", "Drop the shim", 1, sha('5'), ago(25))

	uncertainV := version(1, sha('2'), "Tune the cache", "uncertain")
	uncertainV["request"] = map[string]any{"ref": "refs/valley/requests/uncr", "commit": sha('a')}
	uncertainV["outcome"] = map[string]any{"decision": "failed", "head": sha('2'), "tip": sha('b')}
	landedV := version(1, sha('3'), "Fix the build", "landed")
	landedV["landed_commit"] = sha('3')
	h.writeValleyChanges(map[string]any{
		"schema": "valley-changes/v1",
		"host":   "classic-laddie",
		"repos": []any{map[string]any{
			"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil,
			"changes": []any{
				change(ready, "ready", 1, version(1, sha('1'), "Add a health check", "not-landed")),
				change(uncertain, "ready", 1, uncertainV),
				change(landed, "ready", 1, landedV),
				change(superseded, "ready", 2, version(1, sha('4'), "Rename the flag", "not-landed"),
					version(2, sha('6'), "Rename the flag", "not-landed")),
				change(withdrawn, "withdrawn", 1, version(1, sha('5'), "Drop the shim", "not-landed")),
			},
			"branches": []any{},
		}},
	})

	pane, screen := h.startDashboard(170)
	s := screen("3 yours", "2 done", "READY  v2@6666666", "approve: cd ~/hack/widget && valley approve nxvp --head "+sha('1'))
	for handle, want := range map[string]string{
		"nxvp": "Add a health check",
		"uncr": "Tune the cache",
		"lndd": "Fix the build",
		"sprs": "Rename the flag",
		"wdrn": "Drop the shim",
	} {
		row := rowOf(t, s, handle)
		if f := strings.Fields(strings.TrimPrefix(row, ">")); f[0] != "widget" || !strings.Contains(row, want) || strings.Contains(row, "prompt ") {
			t.Errorf("%s: row should show repo widget, handle and title %q, not the prompt: %q", handle, want, row)
		}
	}
	// Yours first (most recent first), then done.
	labels := []string{"READY  v1@1111111", "UNCERTAIN", "READY  v2@6666666", "INTEGRATED", "WITHDRAWN"}
	var order []string
	for _, line := range strings.Split(s, "\n") {
		for _, l := range labels {
			if strings.Contains(line, l) {
				order = append(order, l)
			}
		}
	}
	if strings.Join(order, ",") != strings.Join(labels, ",") {
		t.Errorf("labels in order %v, want %v:\n%s", order, labels, s)
	}

	// Moving the selection doesn't ask the valley again: one listing per
	// minute, for every run.
	h.tmux("send-keys", "-t", pane, "j")
	screen("> widget  uncr")
	if calls := h.valleyCalls("changes"); len(calls) != 1 || !strings.HasSuffix(calls[0], ": changes --all --json") {
		t.Errorf("valley changes calls = %q, want one", calls)
	}

	res := h.RunKlaus("status")
	if res.ExitCode != 0 {
		t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	for _, tc := range []struct {
		st     *run.State
		status string
	}{
		{ready, "awaiting-approval"},
		{uncertain, "uncertain"},
		{landed, "integrated"},
		{superseded, "superseded"},
		{withdrawn, "withdrawn"},
	} {
		// A valley run's row keeps its run ID, names its change by handle
		// where a PR run shows its number, and shows the change's title.
		if got := statusOf(t, res.Stdout, tc.st.ID); got != tc.status {
			t.Errorf("%s: status %q, want %q", tc.st.Valley.Handle, got, tc.status)
		}
		if got := changeColumn(t, res.Stdout, tc.st.ID); got != tc.st.Valley.Handle {
			t.Errorf("%s: CHANGE %q, want the handle %q", tc.st.ID, got, tc.st.Valley.Handle)
		}
		if line := statusLine(t, res.Stdout, tc.st.ID); !strings.HasSuffix(line, tc.st.Valley.Title) {
			t.Errorf("status row should end with the title %q: %q", tc.st.Valley.Title, line)
		}
	}
}

// TestDashboardValleyFailureFallsBackToGit runs the real dashboard while
// valley can't be reached: valley runs get git's verdicts against their real
// branches, and a run whose change finalize couldn't mark ready says why.
func TestDashboardValleyFailureFallsBackToGit(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub() // no valley.changes.json: changes fails

	landedSHA := h.pushBranch("agent/landed", "l.txt")
	h.git(h.RepoDir, "push", "-q", "origin", landedSHA+":refs/heads/main")
	waitingSHA := h.pushBranch("agent/waiting", "w.txt")
	refusedSHA := h.pushBranch("agent/refused", "r.txt")
	landed := h.valleyRun("20261013-0900-0001", "prompt one", "lndd", "Fix the build", 1, landedSHA, time.Now().Add(-10*time.Minute))
	landed.PushedBranch = strPtrE2E("agent/landed")
	h.SeedState(landed)
	waiting := h.valleyRun("20261013-0900-0002", "prompt two", "wait", "Tune the cache", 2, waitingSHA, time.Now().Add(-5*time.Minute))
	waiting.PushedBranch = strPtrE2E("agent/waiting")
	h.SeedState(waiting)
	refused := h.valleyRun("20261013-0900-0003", "refused one", "", "", 0, refusedSHA, time.Now().Add(-20*time.Minute))
	refused.PushedBranch = strPtrE2E("agent/refused")
	refused.Valley = &run.Valley{Title: "Drop the shim", Error: "valley ready: refused: no principal"}
	h.SeedState(refused)

	_, screen := h.startDashboard(170)
	s := screen("INTEGRATED", "2 yours", "1 done")
	if row := rowOf(t, s, "lndd"); !strings.Contains(row, "Fix the build") || !strings.Contains(row, "INTEGRATED") {
		t.Errorf("landed change should show INTEGRATED from git: %q", row)
	}
	if row := rowOf(t, s, "wait"); !strings.Contains(row, "READY  v2@"+waitingSHA[:7]) {
		t.Errorf("waiting change should still read READY: %q", row)
	}
	var refusedRow string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "refused one") {
			refusedRow = line
		}
	}
	if !strings.Contains(refusedRow, "SUBMIT  agent/refused@"+refusedSHA[:7]) || !strings.Contains(refusedRow, "valley ready: refused: no principal") {
		t.Errorf("a run finalize couldn't mark ready should show SUBMIT and why: %q\n%s", refusedRow, s)
	}
	if calls := h.valleyCalls("changes"); len(calls) != 1 {
		t.Errorf("valley changes calls = %q, want one failed attempt", calls)
	}
}

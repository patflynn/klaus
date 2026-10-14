//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// changeSession is the Claude session the fake claude reports for every run.
const changeSession = "11111111-2222-4333-8444-555555555555"

// recordConversation makes the fake claude also write its conversation where
// claude keeps it, ~/.claude/projects/<cwd, '/' and '.' as '-'>/<session>.jsonl,
// so finalize stores it on the data ref for a later launch to replay.
func (h *Harness) recordConversation() {
	h.t.Helper()
	h.WriteStub("claude", strings.Replace(h.claudeStubScript(), `cat "$dir/claude.output.jsonl"`,
		`conv="$HOME/.claude/projects/$(printf '%s' "$PWD" | tr '/.' '--')"
mkdir -p "$conv"
printf '{"type":"user","sessionId":"`+changeSession+`","cwd":"%s","message":{"role":"user","content":"go"}}\n' "$PWD" > "$conv/`+changeSession+`.jsonl"
cat "$dir/claude.output.jsonl"`, 1))
}

// resetClaude readies the fake claude for another launch: it blocks until
// released again, and its record of the last run is gone.
func (h *Harness) resetClaude() {
	for _, f := range []string{"claude.release", "claude.started", "claude.argv"} {
		os.Remove(filepath.Join(h.E2EDir, f))
	}
}

// launchRun runs klaus launch with args and returns the new run's state and
// the launch's output, failing the test unless launch succeeds.
func (h *Harness) launchRun(args ...string) (*run.State, RunResult) {
	h.t.Helper()
	known := make(map[string]bool)
	for _, id := range h.RunIDs() {
		known[id] = true
	}
	res := h.RunKlaus(append([]string{"launch"}, args...)...)
	if res.ExitCode != 0 {
		h.t.Fatalf("launch %v exited %d\nstdout:\n%s\nstderr:\n%s", args, res.ExitCode, res.Stdout, res.Stderr)
	}
	for _, id := range h.RunIDs() {
		if !known[id] {
			st, err := h.ReadState(id)
			if err != nil {
				h.t.Fatal(err)
			}
			return st, res
		}
	}
	h.t.Fatalf("launch %v recorded no run\nstdout:\n%s", args, res.Stdout)
	return nil, res
}

// pushWork commits a file in a run's worktree and pushes it as the worker
// prompt says, `git push -u origin HEAD`, returning the pushed head.
func (h *Harness) pushWork(st *run.State, file, subject string) string {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(st.Worktree, file), []byte(subject+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.git(st.Worktree, "add", "-A")
	h.git(st.Worktree, "commit", "-qm", subject)
	h.git(st.Worktree, "push", "-q", "-u", "origin", "HEAD")
	return h.GitOutput(st.Worktree, "rev-parse", "HEAD")
}

// finishRun lets the fake claude end and waits for finalize to clean up, so
// the test's temp dirs aren't removed under its push.
func (h *Harness) finishRun(id string) *run.State {
	h.t.Helper()
	h.ReleaseClaude()
	return h.WaitForState(id, func(s *run.State) bool {
		return s.DurationMS != nil && s.TmuxPane == nil && s.Worktree == ""
	}, 60*time.Second)
}

// TestChangeContinuesAChange drives three real launches on one valley
// change, against a fake valley that can unready. The first run pushes v1
// and finalize marks it ready and stores its conversation. A --change
// <run-id> follow-up runs on the first run's branch at v1 with its change
// ID, returns the change to draft, replays the first run's conversation, is
// told it continues the change, and shares the change's dashboard row; it
// pushes fast-forward to the same branch and finalize readies the new head
// there. A --change <handle> --no-replay run resolves the handle through the
// recorded runs, starts at the follow-up's head, and starts fresh.
func TestChangeContinuesAChange(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub()
	h.joinValley()
	if err := os.WriteFile(filepath.Join(h.E2EDir, "valley.has-unready"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.recordConversation()
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")

	first, _ := h.launchRun("add a health check endpoint")
	h.WaitForClaudeStart(30 * time.Second)
	v1 := h.pushWork(first, "health.go", "Add a health check")
	first = h.finishRun(first.ID)
	if first.Valley == nil || first.Valley.Handle != "nxvp" || first.PushedSHA == nil || *first.PushedSHA != v1 {
		t.Fatalf("the first run should have pushed v1 and marked it ready: valley %+v, pushed %v", first.Valley, first.PushedSHA)
	}

	// --change <run-id>.
	h.resetClaude()
	follow, res := h.launchRun("--change", first.ID, "tidy the health check")
	h.WaitForClaudeStart(30 * time.Second)
	for _, want := range []string{
		"Launching agent " + follow.ID + " (continuing change nxvp)...",
		"branch:   " + first.Branch + "\n",
		"change:   " + first.ChangeID + " (inherited from change nxvp)",
		"valley:   widget nxvp is a draft until the agent pushes",
		"replay:   resuming conversation from run " + first.ID,
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("launch output missing %q:\n%s", want, res.Stdout)
		}
	}
	if follow.Branch != first.Branch || follow.ChangeID != first.ChangeID || follow.ChangeRepo != "widget" {
		t.Errorf("follow-up on branch %q, change %q in %q; want %q, %q in widget",
			follow.Branch, follow.ChangeID, follow.ChangeRepo, first.Branch, first.ChangeID)
	}
	if follow.OriginalRunID == nil || *follow.OriginalRunID != first.ID {
		t.Errorf("follow-up should record the run it replays, %s: %v", first.ID, follow.OriginalRunID)
	}
	if follow.Unready == nil || *follow.Unready != (run.Unready{Repo: "widget", Handle: "nxvp"}) {
		t.Errorf("follow-up unready = %+v, want the change back to draft", follow.Unready)
	}
	if _, err := os.Stat(filepath.Join(h.E2EDir, "valley.unready-late")); err == nil {
		t.Error("valley unready ran after the agent started")
	}
	if head := h.GitOutput(follow.Worktree, "rev-parse", "HEAD"); head != v1 {
		t.Errorf("follow-up worktree at %s, want v1 %s", head, v1)
	}
	if argv := h.ClaudeArgv(); !strings.Contains(argv, "--resume\n"+changeSession+"\n") {
		t.Errorf("claude should resume the first run's conversation:\n%s", argv)
	}
	want := "You are continuing an existing change, nxvp. This worktree is on its branch, " + first.Branch + ", which already holds the earlier work."
	if sys := h.WorkerSystemPrompt(); !strings.Contains(sys, want) || !strings.Contains(sys, "push fast-forward to the same branch") {
		t.Errorf("worker prompt should say it continues the change on its branch:\n%s", sys)
	}

	// The follow-up runs on the change's row, as a fix agent runs on its PR's.
	_, screen := h.startDashboard(170)
	running := "agent:" + follow.ID[len(follow.ID)-4:] + " running"
	s := screen("1 agents", running)
	if row := rowOf(t, s, "nxvp"); !strings.Contains(row, running) {
		t.Errorf("the follow-up should run on the change's row: %q\n%s", row, s)
	}
	if strings.Contains(s, "tidy the health check") {
		t.Errorf("the follow-up should have no row of its own:\n%s", s)
	}

	// It pushes fast-forward to the change's branch; finalize readies the new head there.
	worktree := follow.Worktree
	v2 := h.pushWork(follow, "health_test.go", "Test the health check")
	if tip := h.GitOutput(h.OriginDir, "rev-parse", "refs/heads/"+first.Branch); tip != v2 {
		t.Errorf("origin's %s at %s, want the follow-up's %s", first.Branch, tip, v2)
	}
	h.git(h.OriginDir, "merge-base", "--is-ancestor", v1, v2)
	follow = h.finishRun(follow.ID)
	if follow.PushedBranch == nil || *follow.PushedBranch != first.Branch || follow.PushedSHA == nil || *follow.PushedSHA != v2 {
		t.Errorf("follow-up pushed %v at %v, want %s at %s", follow.PushedBranch, follow.PushedSHA, first.Branch, v2)
	}
	ready := worktree + ": ready --change " + first.ChangeID + " --head " + v2 + " " + first.Branch
	if calls := h.valleyCalls("ready"); len(calls) != 2 || calls[1] != ready {
		t.Errorf("valley ready calls = %q, want the second to be %q", calls, ready)
	}

	// --change <handle> --no-replay.
	h.resetClaude()
	third, res := h.launchRun("--change", "nxvp", "--no-replay", "rename the endpoint")
	h.WaitForClaudeStart(30 * time.Second)
	for _, want := range []string{
		"change:   " + first.ChangeID + " (inherited from change nxvp)",
		"replay:   fresh agent (--no-replay)",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("launch output missing %q:\n%s", want, res.Stdout)
		}
	}
	if third.Branch != first.Branch || third.ChangeID != first.ChangeID {
		t.Errorf("third run on branch %q, change %q; want %q, %q", third.Branch, third.ChangeID, first.Branch, first.ChangeID)
	}
	if head := h.GitOutput(third.Worktree, "rev-parse", "HEAD"); head != v2 {
		t.Errorf("third run's worktree at %s, want the follow-up's %s", head, v2)
	}
	if argv := h.ClaudeArgv(); strings.Contains(argv, "--resume") {
		t.Errorf("--no-replay should start a fresh agent:\n%s", argv)
	}
	h.finishRun(third.ID)
}

// TestChangeResolvesThroughValley launches --change with a handle no run
// records: the change was marked ready from another machine, so `valley
// changes --json` names it, its active version's branch and head.
func TestChangeResolvesThroughValley(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub()
	h.joinValley()
	sha := h.pushBranch("readied-elsewhere", "cache.txt")
	changeID := "lmno" + strings.Repeat("k", 28)
	v := version(1, sha, "Tune the cache", "not-landed")
	v["branch"] = "readied-elsewhere"
	h.writeValleyChanges(map[string]any{
		"schema": "valley-changes/v1",
		"host":   "classic-laddie",
		"repos": []any{map[string]any{"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil, "branches": []any{},
			"changes": []any{map[string]any{"change_id": changeID, "handle": "lmno", "target": "main", "owner": "klaus-env",
				"state": "ready", "record": strings.Repeat("0", 40), "seq": 1, "active": 1,
				"versions": []any{v}, "includes": []string{}}}}},
	})
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")

	st, res := h.launchRun("--change", "lmno", "tune the cache further")
	for _, want := range []string{
		"branch:   readied-elsewhere\n",
		"change:   " + changeID + " (inherited from change lmno)",
		"replay:   fresh agent (no prior run recorded for this change)",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("launch output missing %q:\n%s", want, res.Stdout)
		}
	}
	if st.ChangeID != changeID || st.ChangeRepo != "widget" {
		t.Errorf("run's change %q in %q, want %q in widget", st.ChangeID, st.ChangeRepo, changeID)
	}
	if head := h.GitOutput(st.Worktree, "rev-parse", "HEAD"); head != sha {
		t.Errorf("worktree at %s, want the active version's head %s", head, sha)
	}
	if calls := h.valleyCalls("changes"); len(calls) != 1 || !strings.HasSuffix(calls[0], ": changes --json") {
		t.Errorf("valley changes calls = %q, want one for the current repo", calls)
	}
	h.WaitForClaudeStart(30 * time.Second)
	h.finishRun(st.ID)
}

// TestChangeRefErrors launches --change against recorded runs where it must
// refuse before any worktree exists: with --pr, with a handle that names two
// changes, and for a change whose branch is gone from origin.
func TestChangeRefErrors(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	// Launched as --repo widget, the runs' repo.
	h.RegisterProject("acme/widget", h.RepoDir)
	ago := func(m int) time.Time { return time.Now().Add(-time.Duration(m) * time.Minute) }
	a := h.valleyRun("20261013-0900-0001", "add a health check", "nxvp", "Add a health check", 1,
		h.pushBranch("agent/20261013-0900-0001", "a.txt"), ago(20))
	b := h.valleyRun("20261013-0900-0002", "port the health check", "nxvq", "Port the health check", 1,
		h.pushBranch("agent/20261013-0900-0002", "b.txt"), ago(10))
	gone := h.valleyRun("20261013-0900-0003", "drop the shim", "rstu", "Drop the shim", 1, strings.Repeat("3", 40), ago(5))

	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"with --pr", []string{"--pr", "7", "--change", "nxvp"}, []string{"--pr and --change are mutually exclusive"}},
		{"an ambiguous handle", []string{"--change", "nxv"}, []string{`"nxv" names 2 changes in widget`,
			a.ChangeID + " (nxvp), latest run " + a.ID + ": Add a health check",
			b.ChangeID + " (nxvq), latest run " + b.ID + ": Port the health check"}},
		{"a branch gone from origin", []string{"--change", gone.ID}, []string{
			"change rstu's branch agent/20261013-0900-0003 is not on origin (last known head " + strings.Repeat("3", 40) + ")"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := h.RunKlaus(append([]string{"launch", "--repo", "widget"}, append(tc.args, "continue")...)...)
			if res.ExitCode == 0 {
				t.Fatalf("launch should fail\nstdout:\n%s", res.Stdout)
			}
			for _, want := range tc.want {
				if !strings.Contains(res.Stderr, want) {
					t.Errorf("stderr missing %q:\n%s", want, res.Stderr)
				}
			}
		})
	}
	if ids := h.RunIDs(); len(ids) != 3 {
		t.Errorf("refused launches should record no run: %v", ids)
	}
	if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(h.RepoDir), "worktrees")); len(entries) != 0 {
		t.Errorf("refused launches should leave no worktree: %v", entries)
	}
}

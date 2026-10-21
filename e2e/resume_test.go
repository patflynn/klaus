//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// sessionLimitOutput is what the fake claude prints for a run that stops at
// the usage limit: a success result saying so, which finalize salvages as
// session_limit.
const sessionLimitOutput = `{"type":"result","subtype":"success","result":"You've hit your session limit · resets 4:20pm","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n"

// pushToGitHubOrigin is pushWork for a repo whose origin is on GitHub
// (UseGitHubOrigin): git reaches it through the ssh stub, on the harness's
// PATH.
func (h *Harness) pushToGitHubOrigin(st *run.State, file, subject string) string {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(st.Worktree, file), []byte(subject+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.git(st.Worktree, "add", "-A")
	h.git(st.Worktree, "commit", "-qm", subject)
	cmd := exec.Command("git", "push", "-q", "-u", "origin", "HEAD")
	cmd.Dir = st.Worktree
	cmd.Env = append(os.Environ(), "PATH="+h.BinDir+":"+os.Getenv("PATH"), "HOME="+h.Home,
		"GIT_CONFIG_GLOBAL="+filepath.Join(h.Home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("git push: %v: %s", err, out)
	}
	return h.GitOutput(st.Worktree, "rev-parse", "HEAD")
}

// originBranches lists the branches on the sandbox's origin.
func (h *Harness) originBranches() []string {
	h.t.Helper()
	return strings.Fields(h.GitOutput(h.OriginDir, "for-each-ref", "--format=%(refname:short)", "refs/heads"))
}

// TestResumeFromContinuesPushedBranch replays the incident of 2026-10-20
// with real launches, in a repo whose origin is on GitHub. A run stops at
// the usage limit before opening its PR: finalize salvages its unfinished
// work, pushes the branch and marks the run as needing attention. A
// --resume-from follow-up continues that branch with the same change ID and
// is told to push fast-forward and open the PR from it; no other branch
// appears on origin. Once the follow-up opens the PR, klaus status shows the
// salvaged run superseded and names its change by the PR.
func TestResumeFromContinuesPushedBranch(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.UseGitHubOrigin()
	h.recordConversation()
	h.WriteDefaultClaudeOutput(sessionLimitOutput)

	first, _ := h.launchRun("add a health check endpoint")
	h.WaitForClaudeStart(30 * time.Second)
	h.pushToGitHubOrigin(first, "health.go", "Add a health check")
	if err := os.WriteFile(filepath.Join(first.Worktree, "health_test.go"), []byte("cut off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first = h.finishRun(first.ID)
	if first.NeedsAttention == nil || !strings.Contains(*first.NeedsAttention, "session_limit") || first.PRURL != nil {
		t.Fatalf("the first run should be salvaged at the session limit, without a PR: attention %v, PR %v", first.NeedsAttention, first.PRURL)
	}
	if first.PushedBranch == nil || *first.PushedBranch != first.Branch || first.PushedSHA == nil {
		t.Fatalf("finalize should record the salvaged branch on origin: %v at %v", first.PushedBranch, first.PushedSHA)
	}
	salvaged := *first.PushedSHA
	if got := h.GitOutput(h.OriginDir, "show", salvaged+":health_test.go"); got != "cut off" {
		t.Fatalf("origin's %s should hold the salvaged work, got %q", first.Branch, got)
	}

	// The follow-up opens PR #4242 (defaultClaudeOutput). It is later by its
	// creation time too.
	waitNextSecond()
	h.resetWorker()
	h.WriteDefaultClaudeOutput(defaultClaudeOutput)
	follow, res := h.launchRun("--resume-from", first.ID, "carry on")
	h.WaitForClaudeStart(30 * time.Second)
	for _, want := range []string{
		"Launching agent " + follow.ID + " (continuing change " + first.ChangeID + ")...",
		"branch:   " + first.Branch + "\n",
		"resume:   continuing the branch run " + first.ID + " pushed\n",
		"change:   " + first.ChangeID + " ",
		"replay:   resuming conversation from run " + first.ID + " (--resume-from)",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("launch output missing %q:\n%s", want, res.Stdout)
		}
	}
	if follow.Branch != first.Branch || follow.ChangeID != first.ChangeID || follow.DispatchReason != "resume" {
		t.Errorf("follow-up on branch %q, change %q, reason %q; want %q, %q, resume",
			follow.Branch, follow.ChangeID, follow.DispatchReason, first.Branch, first.ChangeID)
	}
	if head := h.GitOutput(follow.Worktree, "rev-parse", "HEAD"); head != salvaged {
		t.Errorf("follow-up worktree at %s, want the salvaged head %s", head, salvaged)
	}
	if got, want := h.originBranches(), []string{first.Branch, "main"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("origin's branches = %v, want %v: no sibling branch", got, want)
	}
	if sys := h.WorkerSystemPrompt(); !strings.Contains(sys, "You are continuing an existing change, "+first.ChangeID+". This worktree is on its branch, "+first.Branch) ||
		!strings.Contains(sys, "push fast-forward to the same branch and open the PR from it") {
		t.Errorf("worker prompt should say it continues the branch and opens the PR from it:\n%s", sys)
	}

	// It pushes fast-forward and opens the PR.
	v2 := h.pushToGitHubOrigin(follow, "health_test.go", "Test the health check")
	h.git(h.OriginDir, "merge-base", "--is-ancestor", salvaged, v2)
	follow = h.finishRun(follow.ID)
	if follow.PRURL == nil || !strings.HasSuffix(*follow.PRURL, "/pull/4242") || follow.NeedsAttention != nil {
		t.Fatalf("the follow-up should open PR #4242: PR %v, attention %v", follow.PRURL, follow.NeedsAttention)
	}

	res = h.RunKlaus("status")
	if res.ExitCode != 0 {
		t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	if got := statusOf(t, res.Stdout, first.ID); got != "superseded" {
		t.Errorf("the salvaged run's status = %q, want superseded\n%s", got, res.Stdout)
	}
	for _, id := range []string{first.ID, follow.ID} {
		if got := changeColumn(t, res.Stdout, id); got != "#4242" {
			t.Errorf("%s: CHANGE %q, want the change's PR #4242", id, got)
		}
	}
}

// TestResumeFromFallsBackToNewBranch: a --resume-from run gets a new branch,
// with the resumed run's change ID, and says why on its resume: line, when
// that run's branch is gone from origin, has diverged from its push, or its
// change went on in a PR. A run that pushed nothing gets a new branch
// without a word.
func TestResumeFromFallsBackToNewBranch(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.RegisterProject("acme/widget", h.RepoDir)
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")
	ago := func(m int) string { return time.Now().Add(-time.Duration(m) * time.Minute).Format(time.RFC3339) }
	clone := h.RepoDir
	salvaged := func(id, branch, sha string, change byte) *run.State {
		st := &run.State{
			ID: id, Type: "launch", Prompt: "stopped at the limit", TargetRepo: strPtrE2E("acme/widget"), CloneDir: &clone,
			Branch: branch, PushedBranch: &branch, PushedSHA: &sha, ChangeID: strings.Repeat(string(change), 32),
			NeedsAttention: strPtrE2E("session_limit; branch " + branch + " preserved (pushed)"), CreatedAt: ago(60),
		}
		h.SeedState(st)
		return st
	}

	gone := salvaged("20261020-1200-00000001", "agent/gone", strings.Repeat("1", 40), 'n')
	diverged := salvaged("20261020-1200-00000002", "agent/diverged", h.pushBranch("agent/elsewhere", "e.txt"), 'o')
	h.pushBranch("agent/diverged", "d.txt")
	inPR := salvaged("20261020-1200-00000003", "agent/in-pr", h.pushBranch("agent/in-pr", "p.txt"), 'p')
	h.SeedState(&run.State{
		ID: "20261020-1200-00000004", Type: "launch", Prompt: "opened the PR", TargetRepo: strPtrE2E("acme/widget"),
		Branch: "agent/in-pr-r", ChangeID: inPR.ChangeID, PRURL: strPtrE2E("https://github.com/acme/widget/pull/335"), CreatedAt: ago(30),
	})
	unpushed := &run.State{
		ID: "20261020-1200-00000005", Type: "launch", Prompt: "pushed nothing", TargetRepo: strPtrE2E("acme/widget"),
		Branch: "agent/unpushed", ChangeID: strings.Repeat("u", 32), CreatedAt: ago(60),
	}
	h.SeedState(unpushed)

	for _, tc := range []struct {
		prior *run.State
		note  string
	}{
		{gone, "run " + gone.ID + "'s branch agent/gone is gone from origin"},
		{diverged, "origin's agent/diverged has diverged from run " + diverged.ID + "'s push " + (*diverged.PushedSHA)[:7]},
		{inPR, "its change went on in PR #335 (run 20261020-1200-00000004); --pr continues that branch"},
		{unpushed, ""},
	} {
		t.Run(tc.prior.Branch, func(t *testing.T) {
			h.resetWorker()
			st, res := h.launchRun("--repo", "widget", "--resume-from", tc.prior.ID, "carry on")
			h.WaitForClaudeStart(30 * time.Second)
			if tc.note != "" && !strings.Contains(res.Stdout, "resume:   new branch: "+tc.note+"\n") {
				t.Errorf("launch output should say why the branch is new, %q:\n%s", tc.note, res.Stdout)
			}
			if tc.note == "" && strings.Contains(res.Stdout, "resume:") {
				t.Errorf("a run that pushed nothing needs no resume: line:\n%s", res.Stdout)
			}
			if st.Branch == tc.prior.Branch || !strings.HasSuffix(st.Branch, runIDTail(st.ID)) || st.ChangeID != tc.prior.ChangeID {
				t.Errorf("follow-up on branch %q, change %q; want a new branch and change %q", st.Branch, st.ChangeID, tc.prior.ChangeID)
			}
			if strings.Contains(res.Stdout, "(continuing change") {
				t.Errorf("a new branch continues no change's branch:\n%s", res.Stdout)
			}
			h.finishRun(st.ID)
		})
	}
}

// TestSupersededAttentionFoldsIntoPR runs the real dashboard, klaus status
// and klaus notifications over the run states of the incident of
// 2026-10-20: two runs salvaged at the usage limit without a PR, each
// followed by a run of its change that opened the PR, one still open and
// one merged; and, for contrast, a salvaged run nothing followed. Each PR is
// one row, without ATTN, and only the run nothing followed needs the
// operator. Status shows the salvaged runs superseded, under their PR, and
// notifications leaves them out of its attention summary.
func TestSupersededAttentionFoldsIntoPR(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteStub("gh", fmt.Sprintf(`#!/usr/bin/env bash
dir=%q
echo "$*" >> "$dir/gh.argv"
ref="${@: -1}"
case "$*" in
  *"--json state "*) case "${ref##*/}" in 335) echo MERGED ;; *) echo OPEN ;; esac ;;
  "pr checks"*) printf 'build\tpending\t0\thttps://ci.invalid/1\n' ;;
  *"--json mergeable "*) echo MERGEABLE ;;
  *"--json isDraft "*) echo false ;;
esac
exit 0
`, h.E2EDir))
	ago := func(m int) string { return time.Now().Add(-time.Duration(m) * time.Minute).Format(time.RFC3339) }
	cost := 3.5
	salvaged := func(id, branch, change string) *run.State {
		st := &run.State{
			ID: id, Type: "launch", Prompt: "stopped at the limit: " + branch, TargetRepo: strPtrE2E("klaus"),
			Branch: branch, PushedBranch: &branch, PushedSHA: strPtrE2E(strings.Repeat("a", 40)), ChangeID: change,
			NeedsAttention: strPtrE2E("session_limit; branch " + branch + " preserved (pushed)"), CostUSD: &cost, CreatedAt: ago(50),
		}
		h.SeedState(st)
		return st
	}
	opened := func(id string, prior *run.State, pr int) *run.State {
		st := &run.State{
			ID: id, Type: "launch", Prompt: "resume after the usage limit", TargetRepo: strPtrE2E("klaus"),
			Branch: prior.Branch + "-r", ChangeID: prior.ChangeID, CostUSD: &cost, CreatedAt: ago(20),
			PRURL: strPtrE2E(fmt.Sprintf("https://github.com/patflynn/klaus/pull/%d", pr)), OriginalRunID: &prior.ID,
		}
		h.SeedState(st)
		return st
	}
	salvagedA := salvaged("20261020-1233-acadffb3", "finalize-s-salvage-never-commits-build-acadffb3", "putlmlnosqkmumnnxyzyxovumsvuvzsz")
	openedA := opened("20261021-0050-1f0b6987", salvagedA, 334)
	salvagedB := salvaged("20261020-1217-9a50467d", "events-say-where-agent-time-goes-and-a-9a50467d", "ymzrntpopqlxywsukrvmqwsunnoqylvv")
	openedB := opened("20261021-0050-24e5aa1e", salvagedB, 335)
	lone := salvaged("20261020-1300-0e0e0e0e", "nobody-followed-up-0e0e0e0e", "qqqqrrrrssssttttuuuuvvvvwwwwxxxx")

	events := ""
	for _, st := range []*run.State{salvagedA, salvagedB, lone} {
		line, err := json.Marshal(map[string]any{"timestamp": st.CreatedAt, "run_id": st.ID, "type": "agent:needs-attention",
			"data": map[string]any{"id": st.ID, "branch": st.Branch, "pushed": true, "reason": "session_limit"}})
		if err != nil {
			t.Fatal(err)
		}
		events += string(line) + "\n"
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(h.RunsDir()), "events.jsonl"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}

	_, screen := h.startDashboard(170)
	s := screen("#334", "#335", "MERGED", "1 yours", "1 pipeline", "1 done")
	for _, num := range []string{"#334", "#335"} {
		var rows []string
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, num+" ") {
				rows = append(rows, line)
			}
		}
		if len(rows) != 1 || strings.Contains(rows[0], "ATTN") {
			t.Errorf("%s should be one row, without ATTN: %q\n%s", num, rows, s)
		}
	}
	for _, change := range []string{salvagedA.ChangeID, salvagedB.ChangeID} {
		if strings.Contains(s, change[:8]) {
			t.Errorf("change %s should have no row of its own:\n%s", change[:8], s)
		}
	}
	if line := lineContaining(s, lone.ChangeID[:8]); !strings.Contains(line, "ATTN") {
		t.Errorf("salvage nothing followed should still read ATTN: %q\n%s", line, s)
	}

	res := h.RunKlaus("status")
	if res.ExitCode != 0 {
		t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	for _, tc := range []struct {
		st             *run.State
		status, change string
	}{
		{salvagedA, "superseded", "#334"},
		{openedA, "pr-created", "#334"},
		{salvagedB, "superseded", "#335"},
		{openedB, "merged", "#335"},
		{lone, "needs-attention", lone.ChangeID[:8]},
	} {
		if got := statusOf(t, res.Stdout, tc.st.ID); got != tc.status {
			t.Errorf("%s: status %q, want %q", tc.st.ID, got, tc.status)
		}
		if got := changeColumn(t, res.Stdout, tc.st.ID); got != tc.change {
			t.Errorf("%s: CHANGE %q, want %q", tc.st.ID, got, tc.change)
		}
	}

	res = h.RunKlaus("notifications", "--all")
	if res.ExitCode != 0 {
		t.Fatalf("notifications exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "1 agent(s) need attention:\n  0e0e0e0e — session_limit\n") ||
		strings.Contains(res.Stdout, "acadffb3") || strings.Contains(res.Stdout, "9a50467d") {
		t.Errorf("notifications should list only the salvage nothing followed:\n%s", res.Stdout)
	}
}

// lineContaining returns the first line of s that contains sub.
func lineContaining(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}

// TestResumeFromContinuesValleyChange: in a repo that delivers to a valley,
// a --resume-from follow-up of a run whose push the valley holds as its
// change's active version continues that branch as --change does: it
// returns the change to draft, pushes fast-forward, and finalize marks the
// new head ready on the same branch. Once the valley's active version is on
// another branch, a --resume-from of the first run gets a new branch and
// says the change moved on.
func TestResumeFromContinuesValleyChange(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.RegisterProject("acme/widget", h.RepoDir)
	h.installValleyStub()
	h.joinValley()
	if err := os.WriteFile(filepath.Join(h.E2EDir, "valley.has-unready"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")

	first, _ := h.launchRun("add a health check endpoint")
	h.WaitForClaudeStart(30 * time.Second)
	v1 := h.pushWork(first, "health.go", "Add a health check")
	first = h.finishRun(first.ID)
	if first.Valley == nil || first.Valley.Handle != "nxvp" {
		t.Fatalf("finalize should mark the first run's push ready: %+v", first.Valley)
	}
	writeListing := func(branch, head string) {
		v := version(1, head, "Add a health check", "not-landed")
		v["branch"] = branch
		h.writeValleyChanges(map[string]any{
			"schema": "valley-changes/v1",
			"host":   "classic-laddie",
			"repos": []any{map[string]any{"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil, "branches": []any{},
				"changes": []any{change(first, "ready", 1, v)}}},
		})
	}
	writeListing(first.Branch, v1)

	h.resetWorker()
	follow, res := h.launchRun("--repo", "widget", "--resume-from", first.ID, "tidy the health check")
	h.WaitForClaudeStart(30 * time.Second)
	for _, want := range []string{
		"Launching agent " + follow.ID + " (continuing change nxvp)...",
		"branch:   " + first.Branch + "\n",
		"resume:   continuing the branch run " + first.ID + " pushed\n",
		"valley:   widget nxvp is a draft until the agent pushes",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("launch output missing %q:\n%s", want, res.Stdout)
		}
	}
	if follow.Branch != first.Branch || follow.ChangeID != first.ChangeID || follow.ChangeRepo != "widget" {
		t.Errorf("follow-up on branch %q, change %q in %q; want %q, %q in widget",
			follow.Branch, follow.ChangeID, follow.ChangeRepo, first.Branch, first.ChangeID)
	}
	worktree := follow.Worktree
	v2 := h.pushWork(follow, "health_test.go", "Test the health check")
	follow = h.finishRun(follow.ID)
	ready := worktree + ": ready --change " + first.ChangeID + " --head " + v2 + " " + first.Branch
	if calls := h.valleyCalls("ready"); len(calls) != 2 || calls[1] != ready {
		t.Errorf("valley ready calls = %q, want the second to be %q", calls, ready)
	}

	// The change's active version moves to another branch.
	h.resetWorker()
	writeListing("readied-elsewhere", h.pushBranch("readied-elsewhere", "cache.txt"))
	other, res := h.launchRun("--repo", "widget", "--resume-from", first.ID, "try again")
	h.WaitForClaudeStart(30 * time.Second)
	if want := "resume:   new branch: change nxvp moved on to branch readied-elsewhere; --change continues it there\n"; !strings.Contains(res.Stdout, want) {
		t.Errorf("launch output missing %q:\n%s", want, res.Stdout)
	}
	if other.Branch == first.Branch || other.ChangeID != first.ChangeID {
		t.Errorf("run on branch %q, change %q; want a new branch and change %q", other.Branch, other.ChangeID, first.ChangeID)
	}
	h.finishRun(other.ID)
}

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/git"
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

// stubGitHubPRs makes gh answer the REST query for the PRs whose head is a
// branch of acme/widget: prs maps a branch to the URL of the PR it heads,
// or to "down" for a query that fails; any other branch heads none. Every
// other gh call answers nothing, as the default stub does.
func (h *Harness) stubGitHubPRs(prs map[string]string) {
	h.t.Helper()
	var cases strings.Builder
	for branch, pr := range prs {
		head := url.QueryEscape("acme:" + branch)
		if pr == "down" {
			fmt.Fprintf(&cases, "    *head=%s) echo 'HTTP 502: Bad Gateway' >&2; exit 1 ;;\n", head)
		} else {
			fmt.Fprintf(&cases, "    *head=%s) echo '[{\"html_url\":\"%s\"}]' ;;\n", head, pr)
		}
	}
	h.WriteStub("gh", fmt.Sprintf(`#!/usr/bin/env bash
dir=%q
{ printf 'gh'; for a in "$@"; do printf ' %%q' "$a"; done; printf '\n'; } >> "$dir/gh.argv"
if [ "$1" = api ]; then
  case "$2" in
%s    repos/*/pulls\?*) echo '[]' ;;
  esac
fi
exit 0
`, h.E2EDir, cases.String()))
}

// seedSalvaged seeds a finished run of change, in acme/widget, that stopped
// at the usage limit: finalize pushed branch at sha and marked it as
// needing attention.
func (h *Harness) seedSalvaged(id, branch, sha, change string) *run.State {
	h.t.Helper()
	clone, cost := h.RepoDir, 0.5
	st := &run.State{
		ID: id, Type: "launch", Prompt: "stopped at the limit", TargetRepo: strPtrE2E("acme/widget"), CloneDir: &clone,
		Branch: branch, PushedBranch: &branch, PushedSHA: &sha, ChangeID: change, CostUSD: &cost,
		NeedsAttention: strPtrE2E("session_limit; branch " + branch + " preserved (pushed)"),
		CreatedAt:      time.Now().Add(-time.Hour).Format(time.RFC3339),
	}
	h.SeedState(st)
	return st
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
	h.stubGitHubPRs(nil)
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
		"takes on: the work of " + first.ID + ", which needed attention\n",
		"replay:   resuming conversation from run " + first.ID + " (--resume-from)",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("launch output missing %q:\n%s", want, res.Stdout)
		}
	}
	if follow.Branch != first.Branch || follow.ChangeID != first.ChangeID || follow.DispatchReason != "resume" ||
		strings.Join(follow.TookOver, ",") != first.ID {
		t.Errorf("follow-up on branch %q, change %q, reason %q, taking on %v; want %q, %q, resume, %s",
			follow.Branch, follow.ChangeID, follow.DispatchReason, follow.TookOver, first.Branch, first.ChangeID, first.ID)
	}
	if calls := h.GHArgv(); !strings.Contains(calls, "gh api repos/acme/widget/pulls\\?state=all") {
		t.Errorf("launch should ask GitHub for the branch's PRs before continuing it:\n%s", calls)
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
// and klaus notifications over constructed run states:
//
//   - a run salvaged at the usage limit and a run of its change that
//     recorded taking its work on (TookOver) and opened PR #334: one row,
//     without ATTN, and the salvaged run superseded;
//   - the incident of 2026-10-20 as it happened: the successor recreated
//     the work on a -r branch and recorded no handoff, and its PR #335
//     merged; the salvaged run still needs attention, on #335's row;
//   - a salvaged run whose successor took its work on and then failed,
//     its clock running behind so it reads as created first: the salvaged
//     run is superseded and the attention is the successor's;
//   - mixed delivery: PR #336 merged, then a run of its change pushed a
//     branch without a PR, which waits on its own SUBMIT row;
//   - a salvaged run nothing followed, which reads ATTN.
func TestSupersededAttentionFoldsIntoPR(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteStub("gh", fmt.Sprintf(`#!/usr/bin/env bash
dir=%q
echo "$*" >> "$dir/gh.argv"
ref="${@: -1}"
case "$*" in
  *"--json state "*) case "${ref##*/}" in 334) echo OPEN ;; *) echo MERGED ;; esac ;;
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
	opened := func(id string, prior *run.State, pr int, branch string, tookOver bool) *run.State {
		st := &run.State{
			ID: id, Type: "launch", Prompt: "resume after the usage limit", TargetRepo: strPtrE2E("klaus"),
			Branch: branch, ChangeID: prior.ChangeID, CostUSD: &cost, CreatedAt: ago(20),
			PRURL: strPtrE2E(fmt.Sprintf("https://github.com/patflynn/klaus/pull/%d", pr)), OriginalRunID: &prior.ID,
		}
		if tookOver {
			st.TookOver = []string{prior.ID}
		}
		h.SeedState(st)
		return st
	}
	handedOff := salvaged("20261021-2300-0a0a0a0a", "continued-on-its-branch-0a0a0a0a", "kkkklllllmmmmmnnnnnooooopppppqqq")
	openedA := opened("20261021-2310-0b0b0b0b", handedOff, 334, handedOff.Branch, true)
	incident := salvaged("20261020-1217-9a50467d", "events-say-where-agent-time-goes-and-a-9a50467d", "ymzrntpopqlxywsukrvmqwsunnoqylvv")
	openedB := opened("20261021-0050-24e5aa1e", incident, 335, incident.Branch+"-r", false)
	failedFrom := salvaged("20261021-2320-0c0c0c0c", "taken-on-then-failed-0c0c0c0c", "rrrrsssssstttttuuuuuvvvvvwwwwwxx")
	failed := &run.State{
		ID: "20261021-2330-0d0d0d0d", Type: "launch", Prompt: "took it on and crashed", TargetRepo: strPtrE2E("klaus"),
		Branch: failedFrom.Branch, ChangeID: failedFrom.ChangeID, TookOver: []string{failedFrom.ID}, CostUSD: &cost,
		FailureReason: strPtrE2E("error_during_execution"), CreatedAt: ago(90), // its clock ran behind
	}
	h.SeedState(failed)
	mergedPR := &run.State{
		ID: "20261021-2340-0e0e0e0e", Type: "launch", Prompt: "opened 336", TargetRepo: strPtrE2E("klaus"), Branch: "mixed",
		ChangeID: "xxxxyyyyyzzzzzkkkkklllllmmmmmnnn", CostUSD: &cost, CreatedAt: ago(40),
		PRURL: strPtrE2E("https://github.com/patflynn/klaus/pull/336"),
	}
	h.SeedState(mergedPR)
	pushedBranch, clone := "mixed-pushed", h.RepoDir
	pushedSHA := h.pushBranch(pushedBranch, "mixed.txt")
	pushedAfter := &run.State{
		ID: "20261021-2350-0f0f0f0f", Type: "launch", Prompt: "pushed after 336 merged", TargetRepo: strPtrE2E("klaus"),
		Branch: pushedBranch, PushedBranch: &pushedBranch, PushedSHA: &pushedSHA, ChangeID: mergedPR.ChangeID,
		CloneDir: &clone, CostUSD: &cost, CreatedAt: ago(10),
	}
	h.SeedState(pushedAfter)
	lone := salvaged("20261020-1300-01010101", "nobody-followed-up-01010101", "qqqqrrrrssssttttuuuuvvvvwwwwxxxx")

	events := ""
	for _, st := range []*run.State{handedOff, incident, failedFrom, failed, lone} {
		line, err := json.Marshal(map[string]any{"timestamp": st.CreatedAt, "run_id": st.ID, "type": "agent:needs-attention",
			"data": map[string]any{"id": st.ID, "branch": st.Branch, "reason": "session_limit"}})
		if err != nil {
			t.Fatal(err)
		}
		events += string(line) + "\n"
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(h.RunsDir()), "events.jsonl"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}

	_, screen := h.startDashboard(170)
	s := screen("#334", "#335", "#336", "SUBMIT", "4 yours", "1 pipeline", "1 done")
	rowsOf := func(sub string) []string {
		var rows []string
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, sub) {
				rows = append(rows, line)
			}
		}
		return rows
	}
	if rows := rowsOf("#334 "); len(rows) != 1 || strings.Contains(rows[0], "ATTN") {
		t.Errorf("#334 should be one row, without ATTN: %q\n%s", rows, s)
	}
	if rows := rowsOf("#335 "); len(rows) != 1 || !strings.Contains(rows[0], "MERGED") || !strings.Contains(rows[0], "ATTN") {
		t.Errorf("#335 should be one row, MERGED with ATTN: its successor recorded no handoff: %q\n%s", rows, s)
	}
	if rows := rowsOf("#336 "); len(rows) != 1 || !strings.Contains(rows[0], "MERGED") || strings.Contains(rows[0], "SUBMIT") {
		t.Errorf("#336 should be one MERGED row: %q\n%s", rows, s)
	}
	if line := lineContaining(s, mergedPR.ChangeID[:8]); !strings.Contains(line, "SUBMIT") || !strings.Contains(line, "pushed after 336 merged") {
		t.Errorf("the push after #336 merged should wait on its change's own row: %q\n%s", line, s)
	}
	for _, st := range []*run.State{failedFrom, lone} {
		if line := lineContaining(s, st.ChangeID[:8]); !strings.Contains(line, "ATTN") {
			t.Errorf("change %s should read ATTN: %q\n%s", st.ChangeID[:8], line, s)
		}
	}

	res := h.RunKlaus("status")
	if res.ExitCode != 0 {
		t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	for _, tc := range []struct {
		st             *run.State
		status, change string
	}{
		{handedOff, "superseded", "#334"},
		{openedA, "pr-created", "#334"},
		{incident, "needs-attention", "#335"},
		{openedB, "merged", "#335"},
		{failedFrom, "superseded", failedFrom.ChangeID[:8]},
		{failed, "needs-attention", failedFrom.ChangeID[:8]},
		{mergedPR, "merged", "#336"},
		{pushedAfter, "awaiting-integration", "#336"},
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
	want := "3 agent(s) need attention:\n  9a50467d — session_limit\n  0d0d0d0d — session_limit\n  01010101 — session_limit\n"
	if !strings.Contains(res.Stdout, want) {
		t.Errorf("notifications should list the runs still needing attention, %q:\n%s", want, res.Stdout)
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

// TestResumeFromFailedSuccessorCarriesAttention: with real launches in a
// repo without PRs, a run stops at the usage limit and a --resume-from run
// continues its branch, recording that it took the work on. The follow-up
// stops at the limit too, and finalize salvages it on the same branch. The
// attention moves to the follow-up: klaus status shows the first run
// superseded and the follow-up needing attention, the dashboard shows the
// change's one row ATTN, and notifications lists only the follow-up.
func TestResumeFromFailedSuccessorCarriesAttention(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteDefaultClaudeOutput(sessionLimitOutput)

	first, _ := h.launchRun("add a health check endpoint")
	h.WaitForClaudeStart(30 * time.Second)
	h.pushWork(first, "health.go", "Add a health check")
	first = h.finishRun(first.ID)
	if first.NeedsAttention == nil || first.PushedSHA == nil {
		t.Fatalf("the first run should be salvaged with its branch pushed: %+v", first)
	}

	h.resetWorker()
	follow, res := h.launchRun("--resume-from", first.ID, "carry on")
	h.WaitForClaudeStart(30 * time.Second)
	if follow.Branch != first.Branch || strings.Join(follow.TookOver, ",") != first.ID {
		t.Fatalf("the follow-up should continue %s, taking on %s: branch %q, took over %v\n%s",
			first.Branch, first.ID, follow.Branch, follow.TookOver, res.Stdout)
	}
	if err := os.WriteFile(filepath.Join(follow.Worktree, "health_test.go"), []byte("cut off again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	follow = h.finishRun(follow.ID)
	if follow.NeedsAttention == nil || !strings.Contains(*follow.NeedsAttention, "session_limit") {
		t.Fatalf("the follow-up should be salvaged at the limit too: %v", follow.NeedsAttention)
	}
	h.git(h.OriginDir, "merge-base", "--is-ancestor", *first.PushedSHA, first.Branch)

	res = h.RunKlaus("status")
	if res.ExitCode != 0 {
		t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	if got := statusOf(t, res.Stdout, first.ID); got != "superseded" {
		t.Errorf("the first run's status = %q, want superseded\n%s", got, res.Stdout)
	}
	if got := statusOf(t, res.Stdout, follow.ID); got != "needs-attention" {
		t.Errorf("the follow-up's status = %q, want needs-attention: the attention is its now\n%s", got, res.Stdout)
	}

	_, screen := h.startDashboard(170)
	s := screen("ATTN", "1 yours")
	if strings.Count(s, "ATTN") != 1 {
		t.Errorf("the change should be one ATTN row:\n%s", s)
	}

	res = h.RunKlaus("notifications", "--all")
	if !strings.Contains(res.Stdout, "1 agent(s) need attention:\n  "+runIDTail(follow.ID)+" — session_limit\n") {
		t.Errorf("notifications should list only the follow-up:\n%s", res.Stdout)
	}
}

// TestResumeFromChecksBranchOwnership: before a --resume-from run continues
// its run's branch on its own, launch checks the branch is still that
// change's. It gets a new branch, and says why, when GitHub has a PR whose
// head is the branch, when GitHub can't be asked, when a run of another
// change worked on the branch, when origin's tip carries another change's
// Change-Id, and when the run is in another owner's repo of the same name.
// A branch that passes every check is continued, though a run in another
// owner's repo of the same name used the same branch name.
func TestResumeFromChecksBranchOwnership(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")
	other := strings.Repeat("z", 32)
	push := map[string]string{}
	for _, b := range []string{"agent/has-pr", "agent/gh-down", "agent/shared", "agent/bobs", "agent/clean"} {
		push[b] = h.pushBranch(b, strings.ReplaceAll(b, "/", "-")+".txt")
	}
	// origin's tip carries another change's commit.
	h.git(h.RepoDir, "checkout", "-q", "-b", "agent/foreign-tip", "origin/main")
	if err := os.WriteFile(filepath.Join(h.RepoDir, "foreign.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.git(h.RepoDir, "add", "foreign.txt")
	h.git(h.RepoDir, "commit", "-qm", "ours", "-m", "Change-Id: "+strings.Repeat("y", 32))
	h.git(h.RepoDir, "push", "-q", "origin", "agent/foreign-tip")
	push["agent/foreign-tip"] = h.GitOutput(h.RepoDir, "rev-parse", "HEAD")
	h.git(h.RepoDir, "checkout", "-q", "main")
	h.git(h.RepoDir, "branch", "-q", "-D", "agent/foreign-tip")

	h.UseGitHubOrigin()
	h.stubGitHubPRs(map[string]string{"agent/has-pr": "https://github.com/acme/widget/pull/77", "agent/gh-down": "down"})
	h.SeedState(&run.State{ID: "20261021-2200-00000009", Type: "launch", Prompt: "another change", TargetRepo: strPtrE2E("acme/widget"),
		Branch: "agent/shared", ChangeID: other, CostUSD: strPtrFloat(0.5), CreatedAt: time.Now().Add(-2 * time.Hour).Format(time.RFC3339)})
	// The same branch name in another owner's widget is no claim on ours.
	h.SeedState(&run.State{ID: "20261021-2200-00000008", Type: "launch", Prompt: "bob's change", TargetRepo: strPtrE2E("bob/widget"),
		RepoKey: "bob/widget", Branch: "agent/clean", ChangeID: other, CostUSD: strPtrFloat(0.5),
		CreatedAt: time.Now().Add(-2 * time.Hour).Format(time.RFC3339)})
	bobs := h.seedSalvaged("20261021-2200-00000005", "agent/bobs", push["agent/bobs"], strings.Repeat("o", 32))
	bobs.TargetRepo, bobs.RepoKey = strPtrE2E("bob/widget"), "bob/widget"
	h.SeedState(bobs)

	for i, tc := range []struct {
		branch, note string
	}{
		{"agent/has-pr", "branch agent/has-pr has PR #77 on GitHub; --pr continues it"},
		{"agent/gh-down", "couldn't ask GitHub whether branch agent/gh-down has a PR: "},
		{"agent/shared", "branch agent/shared is also run 20261021-2200-00000009's, of another change"},
		{"agent/foreign-tip", "origin's agent/foreign-tip ends in a commit of another change, " + strings.Repeat("y", 32)},
		{"agent/bobs", "run 20261021-2200-00000005 is in bob's widget, not acme/widget"},
		{"agent/clean", ""},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			prior := bobs
			if tc.branch != "agent/bobs" {
				prior = h.seedSalvaged(fmt.Sprintf("20261021-2200-0000001%d", i), tc.branch, push[tc.branch], strings.Repeat(string(rune('k'+i)), 32))
			}
			h.resetWorker()
			st, res := h.launchRun("--resume-from", prior.ID, "carry on")
			h.WaitForClaudeStart(30 * time.Second)
			if tc.note == "" {
				if st.Branch != tc.branch || !strings.Contains(res.Stdout, "resume:   continuing the branch run "+prior.ID+" pushed\n") {
					t.Errorf("a branch that passes every check should be continued: on %q\n%s", st.Branch, res.Stdout)
				}
			} else {
				if !strings.Contains(res.Stdout, "resume:   new branch: "+tc.note) {
					t.Errorf("launch output should say %q:\n%s", tc.note, res.Stdout)
				}
				if st.Branch == tc.branch || len(st.TookOver) != 0 {
					t.Errorf("follow-up on %q, taking on %v; want a new branch, taking nothing on", st.Branch, st.TookOver)
				}
			}
			h.finishRun(st.ID)
		})
	}
	if tip := h.GitOutput(h.OriginDir, "rev-parse", "refs/heads/agent/has-pr"); tip != push["agent/has-pr"] {
		t.Errorf("a branch launch didn't continue should be untouched: %s, want %s", tip, push["agent/has-pr"])
	}
}

func strPtrFloat(f float64) *float64 { return &f }

// gitBarrier holds one klaus process at a git call of the test's choosing,
// to force an interleaving: the git on the harness's PATH is a wrapper
// that runs the real one, and in a process started with the barrier's env
// (pause) it stops at the nth call whose arguments match a glob until the
// test releases it. Other processes run git straight through.
type gitBarrier struct {
	t   *testing.T
	dir string
}

// gitBarrier installs the wrapper and returns a new barrier.
func (h *Harness) gitBarrier() *gitBarrier {
	h.t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		h.t.Fatal(err)
	}
	h.WriteStub("git", fmt.Sprintf(`#!/usr/bin/env bash
real=%q
pause() {
  local dir=$E2E_GIT_BARRIER n
  n=$(( $(cat "$dir/count" 2>/dev/null || echo 0) + 1 ))
  echo "$n" > "$dir/count"
  [ "$n" = "$E2E_GIT_PAUSE_NTH" ] || return 0
  : > "$dir/paused"
  # klaus gives a local git call 30s.
  for _ in $(seq 1 500); do [ -f "$dir/release" ] && return 0; sleep 0.05; done
}
if [ -n "$E2E_GIT_BARRIER" ] && [[ "$*" == $E2E_GIT_PAUSE ]]; then
  if [ "$E2E_GIT_PAUSE_AFTER" = 1 ]; then
    "$real" "$@"; rc=$?; pause; exit $rc
  fi
  pause
fi
exec "$real" "$@"
`, real))
	dir, err := os.MkdirTemp(h.E2EDir, "git-barrier-")
	if err != nil {
		h.t.Fatal(err)
	}
	return &gitBarrier{t: h.t, dir: dir}
}

// pause is the environment that holds a process at the nth git call whose
// arguments ("$*") match glob, before the real git runs or, with after,
// once it has.
func (b *gitBarrier) pause(glob string, nth int, after bool) []string {
	env := []string{"E2E_GIT_BARRIER=" + b.dir, "E2E_GIT_PAUSE=" + glob, fmt.Sprintf("E2E_GIT_PAUSE_NTH=%d", nth)}
	if after {
		env = append(env, "E2E_GIT_PAUSE_AFTER=1")
	}
	return env
}

// waitPaused waits until p is held at the barrier, failing the test if p
// exits first.
func (b *gitBarrier) waitPaused(p *klausProcess) {
	b.t.Helper()
	p.waitFor(func() bool {
		_, err := os.Stat(filepath.Join(b.dir, "paused"))
		return err == nil
	}, "to reach the git barrier")
}

// release lets the held process go on.
func (b *gitBarrier) release() {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.dir, "release"), nil, 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// klausProcess is a klaus invocation running in the background, writing
// its output to files.
type klausProcess struct {
	t              *testing.T
	cmd            *exec.Cmd
	stdout, stderr string
	done           chan struct{}
	err            error
}

// startKlaus starts the real klaus binary as RunKlaus runs it, with extra
// environment, and doesn't wait for it.
func (h *Harness) startKlaus(env []string, args ...string) *klausProcess {
	h.t.Helper()
	dir, err := os.MkdirTemp(h.E2EDir, "klaus-")
	if err != nil {
		h.t.Fatal(err)
	}
	p := &klausProcess{t: h.t, stdout: filepath.Join(dir, "stdout"), stderr: filepath.Join(dir, "stderr"), done: make(chan struct{})}
	out, err := os.Create(p.stdout)
	if err != nil {
		h.t.Fatal(err)
	}
	errf, err := os.Create(p.stderr)
	if err != nil {
		h.t.Fatal(err)
	}
	p.cmd = exec.Command(klausBinary, args...)
	p.cmd.Dir, p.cmd.Env, p.cmd.Stdout, p.cmd.Stderr = h.RepoDir, append(h.env(), env...), out, errf
	if err := p.cmd.Start(); err != nil {
		h.t.Fatalf("starting klaus %v: %v", args, err)
	}
	go func() {
		p.err = p.cmd.Wait()
		out.Close()
		errf.Close()
		close(p.done)
	}()
	h.t.Cleanup(func() {
		p.cmd.Process.Kill()
		<-p.done
	})
	return p
}

// output is what p has written to stdout and stderr so far.
func (p *klausProcess) output() (string, string) {
	out, _ := os.ReadFile(p.stdout)
	errOut, _ := os.ReadFile(p.stderr)
	return string(out), string(errOut)
}

// waitFor waits up to 30s until cond holds, failing the test if p exits
// first; what names what it waits for p to do.
func (p *klausProcess) waitFor(cond func() bool, what string) {
	p.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		select {
		case <-p.done:
			if cond() {
				return
			}
			stdout, stderr := p.output()
			p.t.Fatalf("klaus %v exited before %s\nstdout:\n%s\nstderr:\n%s", p.cmd.Args[1:], what, stdout, stderr)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("klaus %v didn't %s within 30s", p.cmd.Args[1:], what)
		}
	}
}

// wait waits for p to exit and returns its outcome.
func (p *klausProcess) wait() RunResult {
	p.t.Helper()
	<-p.done
	stdout, stderr := p.output()
	res := RunResult{Stdout: stdout, Stderr: stderr}
	if ee, ok := p.err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if p.err != nil {
		p.t.Fatalf("running klaus %v: %v", p.cmd.Args[1:], p.err)
	}
	return res
}

// TestContinuingNeverTouchesOtherWork: a --resume-from run that would
// continue its run's branch refuses, as --change and --pr do, and leaves
// the work where it is, when the branch is checked out in a worktree with
// uncommitted changes, when the local branch has commits origin lacks, when
// a finished run's clean worktree still has it checked out, and, even with
// --force-takeover, when a worktree no run registered has it. Of two such
// launches on one branch, the second checks while the first is between its
// own check and its run's registration, its worktree created and work
// under way in it (held there by a git barrier): the second waits for the
// first to register, then refuses, naming it, and the first's worktree
// keeps its uncommitted work.
func TestContinuingNeverTouchesOtherWork(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.RegisterProject("acme/widget", h.RepoDir)
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")
	cost := 0.5
	// worktree checks branch out at a new path outside the repo.
	worktree := func(branch string) string {
		path := filepath.Join(t.TempDir(), "wt")
		h.git(h.RepoDir, "worktree", "add", "-q", "-B", branch, path, "origin/"+branch)
		return path
	}
	prior := func(i int, branch string) *run.State {
		return h.seedSalvaged(fmt.Sprintf("20261021-2200-0000002%d", i), branch, h.pushBranch(branch, fmt.Sprintf("f%d.txt", i)),
			strings.Repeat(string(rune('k'+i)), 32))
	}

	dirty := prior(0, "agent/dirty")
	dirtyWT := worktree("agent/dirty")
	if err := os.WriteFile(filepath.Join(dirtyWT, "notes.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.SeedState(&run.State{ID: "20261021-2200-00000030", Type: "launch", Prompt: "an earlier attempt", TargetRepo: strPtrE2E("acme/widget"),
		Branch: "agent/dirty", ChangeID: dirty.ChangeID, Worktree: dirtyWT, CostUSD: &cost, CreatedAt: dirty.CreatedAt})

	unpushed := prior(1, "agent/unpushed")
	scratch := worktree("agent/unpushed")
	h.git(scratch, "commit", "-q", "--allow-empty", "-m", "local only")
	h.git(h.RepoDir, "worktree", "remove", scratch)

	occupied := prior(2, "agent/occupied")
	occupiedWT := worktree("agent/occupied")
	h.SeedState(&run.State{ID: "20261021-2200-00000032", Type: "launch", Prompt: "an earlier attempt", TargetRepo: strPtrE2E("acme/widget"),
		Branch: "agent/occupied", ChangeID: occupied.ChangeID, Worktree: occupiedWT, CostUSD: &cost, CreatedAt: occupied.CreatedAt})

	foreign := prior(3, "agent/foreign")
	foreignWT := worktree("agent/foreign")

	for _, tc := range []struct {
		name  string
		prior *run.State
		force bool
		want  string
		keep  string
	}{
		{"dirty worktree", dirty, false, "--resume-from: branch agent/dirty is checked out in run 20261021-2200-00000030's worktree " + dirtyWT + " (with uncommitted changes); klaus never removes a worktree, even with --force-takeover: save that work, then clean that run up (klaus cleanup 20261021-2200-00000030, with --force while its agent runs) and launch again", filepath.Join(dirtyWT, "notes.txt")},
		{"unpushed commits", unpushed, false, `--resume-from: creating worktree: local branch "agent/unpushed" has 1 commit(s) not on origin/agent/unpushed`, ""},
		{"a finished run's worktree", occupied, false, "--resume-from: branch agent/occupied is checked out in run 20261021-2200-00000032's worktree " + occupiedWT + "; klaus never removes a worktree, even with --force-takeover: clean that run up first (klaus cleanup 20261021-2200-00000032, with --force while its agent runs) and launch again", occupiedWT},
		{"a worktree no run registered", foreign, true, "--resume-from: branch agent/foreign is checked out in " + foreignWT + ", which no klaus run registered; klaus never removes it", foreignWT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"launch", "--repo", "widget", "--resume-from", tc.prior.ID}
			if tc.force {
				args = append(args, "--force-takeover")
			}
			res := h.RunKlaus(append(args, "carry on")...)
			if res.ExitCode == 0 {
				t.Fatalf("launch should refuse\nstdout:\n%s", res.Stdout)
			}
			if !strings.Contains(res.Stderr, tc.want) {
				t.Errorf("stderr should say %q:\n%s", tc.want, res.Stderr)
			}
			if tc.keep != "" {
				if _, err := os.Stat(tc.keep); err != nil {
					t.Errorf("%s should be untouched: %v", tc.keep, err)
				}
			}
		})
	}
	if got := h.GitOutput(h.RepoDir, "rev-list", "--count", "origin/agent/unpushed..agent/unpushed"); got != "1" {
		t.Errorf("the unpushed commit should still be on the local branch: %s ahead", got)
	}

	// Two launches on one branch: the first holds once its worktree
	// exists, before its run is registered, and work starts in it; the
	// second launches then.
	contested := prior(4, "agent/contested")
	before := len(h.RunIDs())
	barrier := h.gitBarrier()
	first := h.startKlaus(barrier.pause("worktree add -B agent/contested *", 1, true),
		"launch", "--repo", "widget", "--resume-from", contested.ID, "attempt 1")
	barrier.waitPaused(first)
	firstWT := git.BranchWorktree(context.Background(), h.RepoDir, "agent/contested")
	if firstWT == "" {
		t.Fatal("the first launch's worktree should have agent/contested checked out")
	}
	notes := filepath.Join(firstWT, "notes.txt")
	if err := os.WriteFile(notes, []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := h.startKlaus(nil, "launch", "--repo", "widget", "--resume-from", contested.ID, "attempt 2")
	second.waitFor(func() bool {
		_, stderr := second.output()
		return strings.Contains(stderr, "waiting for another launch continuing a branch in ")
	}, "wait for the first launch")
	barrier.release()
	won, lost := first.wait(), second.wait()
	if won.ExitCode != 0 || lost.ExitCode == 0 {
		t.Fatalf("the first launch should start and the second refuse: exits %d and %d\nfirst:\n%s%s\nsecond:\n%s%s",
			won.ExitCode, lost.ExitCode, won.Stdout, won.Stderr, lost.Stdout, lost.Stderr)
	}
	if ids := h.RunIDs(); len(ids) != before+1 {
		t.Fatalf("one run should be recorded: %d new", len(ids)-before)
	}
	var winner *run.State
	for _, id := range h.RunIDs() {
		if st, _ := h.ReadState(id); st != nil && st.Branch == "agent/contested" && st.TmuxPane != nil {
			winner = st
		}
	}
	if winner == nil || winner.Worktree != firstWT {
		t.Fatalf("the first launch's run should continue agent/contested in %s: %+v\n%s", firstWT, winner, won.Stdout)
	}
	if want := "--resume-from: run " + winner.ID + " is still working on"; !strings.Contains(lost.Stderr, want) {
		t.Errorf("the second launch should refuse, naming %s: %q\n%s", winner.ID, want, lost.Stderr)
	}
	h.WaitForClaudeStart(30 * time.Second)
	if data, err := os.ReadFile(notes); err != nil || string(data) != "half done\n" {
		t.Errorf("the first run's uncommitted work should be intact: %q, %v", data, err)
	}
	if got := h.GitOutput(firstWT, "status", "--porcelain"); got != "?? notes.txt" {
		t.Errorf("the first run's worktree should hold its work and nothing else: %q", got)
	}
	if got := h.GitOutput(firstWT, "rev-parse", "--abbrev-ref", "HEAD"); got != "agent/contested" {
		t.Errorf("the first run's worktree should still be on agent/contested: %s", got)
	}
	os.Remove(notes) // so finalize has nothing to salvage
	h.finishRun(winner.ID)
}

// TestForcedTakeoverNeverRemovesAWorktree: --force-takeover refuses while
// a worktree has the branch checked out, with uncommitted work in it, and
// leaves it as it was: a finished run's worktree, and an ordinary checkout
// recreated at a run's registered path after that run's worktree was
// removed, which no check of the path could tell from the run's. The
// refusal names the run and the work, and no run is recorded.
func TestForcedTakeoverNeverRemovesAWorktree(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.RegisterProject("acme/widget", h.RepoDir)
	cost := 0.5
	for i, tc := range []struct {
		name     string
		recreate bool // the run's worktree is removed, and a checkout made at its path
	}{
		{"a finished run's worktree", false},
		{"a checkout recreated at a run's path", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := fmt.Sprintf("agent/forced-%d", i)
			salvaged := h.seedSalvaged(fmt.Sprintf("20261021-2200-0000004%d", i), branch, h.pushBranch(branch, fmt.Sprintf("forced%d.txt", i)),
				strings.Repeat(string(rune('k'+i)), 32))
			runWT := filepath.Join(t.TempDir(), "wt")
			h.git(h.RepoDir, "worktree", "add", "-q", "-B", branch, runWT, "origin/"+branch)
			holder := fmt.Sprintf("20261021-2200-0000005%d", i)
			h.SeedState(&run.State{ID: holder, Type: "launch", Prompt: "an earlier attempt", TargetRepo: strPtrE2E("acme/widget"),
				Branch: branch, ChangeID: salvaged.ChangeID, Worktree: runWT, CostUSD: &cost, CreatedAt: salvaged.CreatedAt})
			if tc.recreate {
				h.git(h.RepoDir, "worktree", "remove", runWT)
				h.git(h.RepoDir, "worktree", "add", "-q", runWT, branch)
			}
			notes := filepath.Join(runWT, "notes.txt")
			if err := os.WriteFile(notes, []byte("half done\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := len(h.RunIDs())

			res := h.RunKlaus("launch", "--repo", "widget", "--resume-from", salvaged.ID, "--force-takeover", "take it over")
			if res.ExitCode == 0 {
				t.Fatalf("launch should refuse\nstdout:\n%s", res.Stdout)
			}
			want := "--resume-from: branch " + branch + " is checked out in run " + holder + "'s worktree " + runWT +
				" (with uncommitted changes); klaus never removes a worktree, even with --force-takeover: save that work, then clean that run up (klaus cleanup " + holder
			if !strings.Contains(res.Stderr, want) {
				t.Errorf("stderr should say %q:\n%s", want, res.Stderr)
			}
			if data, err := os.ReadFile(notes); err != nil || string(data) != "half done\n" {
				t.Errorf("the uncommitted work should be intact: %q, %v", data, err)
			}
			if got := h.GitOutput(runWT, "status", "--porcelain"); got != "?? notes.txt" {
				t.Errorf("the worktree should hold its work and nothing else: %q", got)
			}
			if got := h.GitOutput(runWT, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
				t.Errorf("the worktree should still be on %s: %s", branch, got)
			}
			if ids := h.RunIDs(); len(ids) != before {
				t.Errorf("a refused launch should record no run: %d new", len(ids)-before)
			}
		})
	}
}

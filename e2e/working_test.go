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

// TestDashboardShowsWorkingWhileFollowUpRuns runs the real dashboard and
// klaus status while a follow-up run of a valley change is running: the
// change's ready row reads WORKING with the agents and offers no approve
// command, the same change ID in another repo is untouched, a draft change
// with no running run reads DRAFT, and once the follow-up exits the row is
// APPROVE again.
func TestDashboardShowsWorkingWhileFollowUpRuns(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub()
	h.AmendRepoConfig(map[string]any{
		"dashboard": map[string]any{"approve_command": "cd ~/hack/{repo} && valley approve {handle} --head {head}"},
	})
	sha := func(c byte) string { return strings.Repeat(string(c), 40) }
	ago := func(m int) time.Time { return time.Now().Add(-time.Duration(m) * time.Minute) }

	ready := h.valleyRun("20261013-0900-0001", "prompt one", "nxvp", "Add a health check", 1, sha('1'), ago(10))
	// The same change ID in another repo: not the change the follow-up
	// continues.
	other := h.valleyRun("20261013-0900-0002", "prompt two", "nxvpk", "Port the health check", 1, sha('2'), ago(5))
	other.ChangeID, other.TargetRepo, other.Valley.Repo = ready.ChangeID, strPtrE2E("acme/gadget"), "gadget"
	h.SeedState(other)
	draft := h.valleyRun("20261013-0900-0004", "prompt four", "drft", "Tune the cache", 1, sha('4'), ago(20))

	// The follow-up: not finalized, on a live pane, so it is running.
	pane := strings.TrimSpace(h.tmux("new-window", "-d", "-P", "-F", "#{pane_id}", "sleep 600"))
	follow := &run.State{
		ID: "20261013-0900-0003", Type: "launch", Prompt: "tidy the health check", TargetRepo: strPtrE2E("acme/widget"),
		Branch: "agent/tidy", Worktree: "/wt", TmuxPane: &pane, ChangeID: ready.ChangeID,
		CreatedAt: ago(1).Format(time.RFC3339),
	}
	h.SeedState(follow)

	otherChange := change(other, "ready", 1, version(1, sha('2'), "Port the health check", "not-landed"))
	h.writeValleyChanges(map[string]any{
		"schema": "valley-changes/v1",
		"host":   "classic-laddie",
		"repos": []any{
			map[string]any{"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil, "branches": []any{},
				"changes": []any{
					change(ready, "ready", 1, version(1, sha('1'), "Add a health check", "not-landed")),
					change(draft, "draft", 1, version(1, sha('4'), "Tune the cache", "not-landed")),
				}},
			map[string]any{"repo": "gadget", "url": "git@classic-laddie:gadget.git", "error": nil, "branches": []any{},
				"changes": []any{otherChange}},
		},
	})

	tmuxPane, screen := h.startDashboard(170)
	s := screen("2 yours", "2 agents", "WORKING", "DRAFT")
	if row := rowOf(t, s, "nxvp"); !strings.Contains(row, "WORKING  v1@1111111") || strings.Contains(row, "APPROVE") {
		t.Errorf("the ready change should read WORKING while the follow-up runs: %q", row)
	}
	if row := rowOf(t, s, "nxvpk"); !strings.HasPrefix(strings.TrimPrefix(row, "> "), "gadget") || !strings.Contains(row, "APPROVE  v1@2222222") {
		t.Errorf("the same change ID in another repo should still read APPROVE: %q", row)
	}
	if row := rowOf(t, s, "drft"); !strings.Contains(row, "DRAFT  v1@4444444") {
		t.Errorf("a draft change with no running run should read DRAFT: %q", row)
	}
	// Yours first (APPROVE, then DRAFT), then the agents: the follow-up,
	// then the row it is working on, which is last.
	var order []string
	for _, line := range strings.Split(s, "\n") {
		for _, l := range []string{"APPROVE", "DRAFT", "RUNNING", "WORKING"} {
			if strings.Contains(line, l+"  ") {
				order = append(order, l)
			}
		}
	}
	if strings.Join(order, ",") != "APPROVE,DRAFT,RUNNING,WORKING" {
		t.Errorf("labels in order %v, want APPROVE,DRAFT,RUNNING,WORKING:\n%s", order, s)
	}

	// The DRAFT row offers no approve command either.
	h.tmux("send-keys", "-t", tmuxPane, "j")
	s = screen("> widget  drft")
	if strings.Contains(s, "approve:") {
		t.Errorf("a DRAFT row should offer no approve command:\n%s", s)
	}

	h.tmux("send-keys", "-t", tmuxPane, "G")
	s = screen("> widget  nxvp ", "agent:0003 is still working on this change")
	if strings.Contains(s, "approve:") {
		t.Errorf("a WORKING row should offer no approve command:\n%s", s)
	}

	res := h.RunKlaus("status")
	if res.ExitCode != 0 {
		t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	for handle, want := range map[string]string{"nxvp": "working", "nxvpk": "awaiting-approval", "drft": "draft"} {
		if got := statusOf(t, res.Stdout, handle); got != want {
			t.Errorf("%s: status %q, want %q", handle, got, want)
		}
	}
	if got := statusOf(t, res.Stdout, follow.ID); got != "running" {
		t.Errorf("follow-up: status %q, want running", got)
	}

	// The follow-up exits: the row is the operator's again, with its command.
	h.tmux("kill-pane", "-t", pane)
	cost, dur := 0.5, int64(1000)
	follow.TmuxPane, follow.CostUSD, follow.DurationMS = nil, &cost, &dur
	h.SeedState(follow)
	s = screen("3 yours", "approve: cd ~/hack/widget && valley approve nxvp --head "+sha('1'))
	if row := rowOf(t, s, "nxvp"); !strings.HasPrefix(row, "> ") || !strings.Contains(row, "APPROVE  v1@1111111") {
		t.Errorf("after the follow-up exits the row should be selected and read APPROVE: %q\n%s", row, s)
	}
	if strings.Contains(s, "WORKING") || strings.Contains(s, "still working") {
		t.Errorf("nothing should read WORKING once the follow-up exits:\n%s", s)
	}
}

// TestFollowUpLaunchUnreadiesValleyChange launches a --resume-from follow-up
// of a valley change and lets the real finalize run, against a fake valley:
// when valley lists unready, launch returns the change to draft before the
// agent starts and finalize marks the pushed head ready again; without the
// verb launch never runs it; and a refused unready is recorded and warned
// about without failing the launch.
func TestFollowUpLaunchUnreadiesValleyChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unready bool
		refuse  string
	}{
		{name: "valley lists unready", unready: true},
		{name: "valley without unready"},
		{name: "unready refused", unready: true, refuse: "valley: refused: nxvp has a pending request\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.installValleyStub()
			h.joinValley()
			if tc.unready {
				if err := os.WriteFile(filepath.Join(h.E2EDir, "valley.has-unready"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.refuse != "" {
				if err := os.WriteFile(filepath.Join(h.E2EDir, "valley.unready-refuse"), []byte(tc.refuse), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			prior := h.valleyRun("20261013-0900-0001", "add a health check", "nxvp", "Add a health check", 1,
				strings.Repeat("1", 40), time.Now().Add(-time.Hour))
			h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"11111111-2222-4333-8444-555555555555"}` + "\n")

			res := h.RunKlaus("launch", "--resume-from", prior.ID, "tidy the health check")
			if res.ExitCode != 0 {
				t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
			}
			var st *run.State
			for _, id := range h.RunIDs() {
				if id != prior.ID {
					st, _ = h.ReadState(id)
				}
			}
			if st == nil || st.ChangeID != prior.ChangeID {
				t.Fatalf("follow-up state %+v, want change %s\nstdout:\n%s", st, prior.ChangeID, res.Stdout)
			}
			if st.ChangeRepo != "widget" {
				t.Errorf("follow-up recorded change repo %q, want the prior run's %q", st.ChangeRepo, "widget")
			}
			h.WaitForClaudeStart(30 * time.Second)

			calls := h.valleyCalls("unready")
			switch {
			case !tc.unready:
				if len(calls) != 0 || st.Unready != nil {
					t.Errorf("a valley without unready must not be asked to unready: calls %q, recorded %+v", calls, st.Unready)
				}
			case tc.refuse != "":
				want := "valley unready: valley: refused: nxvp has a pending request"
				if st.Unready == nil || st.Unready.Error != want || st.Unready.Handle != "" {
					t.Errorf("refusal recorded as %+v, want error %q", st.Unready, want)
				}
				if !strings.Contains(res.Stderr, "warning: could not return the change to draft: "+want) {
					t.Errorf("launch should warn about the refusal:\n%s", res.Stderr)
				}
			default:
				want := st.Worktree + ": unready " + prior.ChangeID
				if len(calls) != 1 || calls[0] != want {
					t.Errorf("valley unready calls = %q, want [%q]", calls, want)
				}
				if st.Unready == nil || *st.Unready != (run.Unready{Repo: "widget", Handle: "nxvp"}) {
					t.Errorf("recorded unready = %+v", st.Unready)
				}
				if !strings.Contains(res.Stdout, "valley:   widget nxvp is a draft until the agent pushes") {
					t.Errorf("launch should say the change is a draft:\n%s", res.Stdout)
				}
			}
			if _, err := os.Stat(filepath.Join(h.E2EDir, "valley.unready-late")); err == nil {
				t.Error("valley unready ran after the agent started")
			}

			// The agent pushes; finalize marks the head ready again.
			if err := os.WriteFile(filepath.Join(st.Worktree, "health.go"), []byte("package health\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			h.git(st.Worktree, "add", "-A")
			h.git(st.Worktree, "commit", "-qm", "Tidy the health check")
			h.git(st.Worktree, "push", "-q", "origin", st.Branch)
			head := h.GitOutput(st.Worktree, "rev-parse", "HEAD")
			worktree := st.Worktree
			h.ReleaseClaude()
			h.WaitForState(st.ID, func(s *run.State) bool {
				return s.DurationMS != nil && s.TmuxPane == nil && s.Worktree == ""
			}, 60*time.Second)
			want := worktree + ": ready --change " + prior.ChangeID + " --head " + head + " " + st.Branch
			if calls := h.valleyCalls("ready"); len(calls) != 1 || calls[0] != want {
				t.Errorf("valley ready calls = %q, want [%q]", calls, want)
			}
		})
	}
}

// TestFollowUpOfRenamedRepoShowsWorking launches a --resume-from follow-up
// of a valley change from a target repo, acme/source, that the valley calls
// widget, as the prior run recorded, against a valley that can't unready: the
// follow-up records the repo of the run it continues, so the prior row reads
// WORKING while it runs.
func TestFollowUpOfRenamedRepoShowsWorking(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub()
	h.joinValley()
	// Registered as the project "source", which --repo names.
	h.RegisterProject("acme/source", h.RepoDir)
	sha := strings.Repeat("1", 40)
	prior := h.valleyRun("20261013-0900-0001", "add a health check", "nxvp", "Add a health check", 1, sha,
		time.Now().Add(-time.Hour))
	prior.TargetRepo = strPtrE2E("acme/source")
	h.SeedState(prior)
	h.writeValleyChanges(map[string]any{
		"schema": "valley-changes/v1",
		"host":   "classic-laddie",
		"repos": []any{
			map[string]any{"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil, "branches": []any{},
				"changes": []any{change(prior, "ready", 1, version(1, sha, "Add a health check", "not-landed"))}},
		},
	})
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"11111111-2222-4333-8444-555555555555"}` + "\n")

	res := h.RunKlaus("launch", "--repo", "source", "--resume-from", prior.ID, "tidy the health check")
	if res.ExitCode != 0 {
		t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var st *run.State
	for _, id := range h.RunIDs() {
		if id != prior.ID {
			st, _ = h.ReadState(id)
		}
	}
	if st == nil || st.ChangeID != prior.ChangeID {
		t.Fatalf("follow-up state %+v, want change %s\nstdout:\n%s", st, prior.ChangeID, res.Stdout)
	}
	if st.TargetRepo == nil || *st.TargetRepo != "source" || st.Unready != nil || len(h.valleyCalls("unready")) != 0 {
		t.Fatalf("follow-up should target source without unready: target %v, unready %+v, calls %q",
			st.TargetRepo, st.Unready, h.valleyCalls("unready"))
	}
	if st.ChangeRepo != "widget" {
		t.Errorf("follow-up recorded change repo %q, want the prior run's %q", st.ChangeRepo, "widget")
	}
	h.WaitForClaudeStart(30 * time.Second)

	_, screen := h.startDashboard(170)
	s := screen("WORKING")
	if row := rowOf(t, s, "nxvp"); !strings.Contains(row, "WORKING  v1@1111111") {
		t.Errorf("the prior row should read WORKING while its follow-up runs: %q\n%s", row, s)
	}
	status := h.RunKlaus("status")
	if got := statusOf(t, status.Stdout, "nxvp"); got != "working" {
		t.Errorf("prior: status %q, want working", got)
	}

	h.ReleaseClaude()
	h.WaitForState(st.ID, func(s *run.State) bool { return s.DurationMS != nil }, 60*time.Second)
}

// TestStatusJudgesPanesAsTheDashboardDoes runs the real klaus status while a
// follow-up of a valley change keeps its pane but no longer runs: the pane is
// dead (its command exited and tmux kept it), or the run finalized and its
// pane sits idle at a shell. Status judges panes as the dashboard does, so
// the follow-up isn't running and the change's own run is the operator's
// again, not working.
func TestStatusJudgesPanesAsTheDashboardDoes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		finalized bool
	}{
		{name: "dead pane"},
		{name: "idle pane after finalize", finalized: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.installValleyStub()
			sha := strings.Repeat("1", 40)
			ready := h.valleyRun("20261013-0900-0001", "prompt one", "nxvp", "Add a health check", 1, sha,
				time.Now().Add(-10*time.Minute))
			h.writeValleyChanges(map[string]any{
				"schema": "valley-changes/v1",
				"host":   "classic-laddie",
				"repos": []any{
					map[string]any{"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil, "branches": []any{},
						"changes": []any{change(ready, "ready", 1, version(1, sha, "Add a health check", "not-landed"))}},
				},
			})

			pane := strings.TrimSpace(h.tmux("new-window", "-d", "-P", "-F", "#{pane_id}", "sleep 600"))
			follow := &run.State{
				ID: "20261013-0900-0002", Type: "launch", Prompt: "tidy the health check", TargetRepo: strPtrE2E("acme/widget"),
				Branch: "agent/tidy", Worktree: "/wt", TmuxPane: &pane, ChangeID: ready.ChangeID,
				CreatedAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
			}
			if tc.finalized {
				cost, dur := 0.5, int64(1000)
				follow.CostUSD, follow.DurationMS = &cost, &dur
			}
			h.SeedState(follow)
			statuses := func() (string, string) {
				t.Helper()
				res := h.RunKlaus("status")
				if res.ExitCode != 0 {
					t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
				}
				return statusOf(t, res.Stdout, follow.ID), statusOf(t, res.Stdout, "nxvp")
			}
			// While the pane's command runs, the follow-up is running.
			if f, r := statuses(); f != "running" || r != "working" {
				t.Fatalf("with the pane busy: follow-up %q, change %q; want running, working", f, r)
			}

			if tc.finalized {
				// A shell waiting at its prompt.
				h.tmux("respawn-pane", "-k", "-t", pane, "exec bash --noprofile --norc")
				waitForPane(t, h, pane, "#{pane_current_command}", "bash")
			} else {
				h.tmux("set-option", "-w", "-t", pane, "remain-on-exit", "on")
				h.tmux("respawn-pane", "-k", "-t", pane, "true")
				waitForPane(t, h, pane, "#{pane_dead}", "1")
			}
			if !h.PaneExists(pane) {
				t.Fatalf("pane %s should still exist", pane)
			}
			if f, r := statuses(); f != "exited" || r != "awaiting-approval" {
				t.Errorf("follow-up %q, change %q; want exited, awaiting-approval", f, r)
			}
		})
	}
}

// waitForPane waits until format, displayed for pane, reads want.
func waitForPane(t *testing.T, h *Harness, pane, format, want string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got = strings.TrimSpace(h.tmux("display-message", "-p", "-t", pane, format)); got == want {
			return
		}
	}
	t.Fatalf("pane %s: %s is %q, want %q", pane, format, got, want)
}

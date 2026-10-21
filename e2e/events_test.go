//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/report"
	"github.com/patflynn/klaus/internal/run"
)

// TestEventsSayWhereAgentTimeGoes launches a run whose agent runs go test and
// opens PR 12, then a CI fix on that PR as the pipeline launches one (--pr 12
// --reason ci-fix), and lets both finalize. Every event of both runs carries
// the repo-qualified keys, the head SHA and the dispatch reason; each run's
// phases and the test run in its stream are events, in order; and klaus
// report time sums the two runs as one change with a ci-fix follow-up.
func TestEventsSayWhereAgentTimeGoes(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteStub("gh", reviewerGHStub(h, false))
	h.git(h.RepoDir, "push", "-q", "origin", "main:refs/heads/fix-branch")
	h.UseGitHubOrigin()
	const prURL = "https://github.com/acme/widget/pull/12"
	h.WriteDefaultClaudeOutput(`{"type":"system","subtype":"init","model":"claude-e2e"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":"ok"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Opened PR: ` + prURL + `"}]}}
{"type":"result","subtype":"success","total_cost_usd":0.25,"duration_ms":4242,"session_id":"11111111-2222-4333-8444-555555555555"}
`)

	first, _ := h.launchRun("# Add a health check\n\nAdd a /healthz endpoint.")
	h.WaitForClaudeStart(30 * time.Second)
	first = h.finishRun(first.ID)
	if first.PRURL == nil || *first.PRURL != prURL || first.DispatchReason != event.DispatchLaunch || first.RepoKey != "acme/widget" {
		t.Fatalf("first run: PR %v, reason %q, repo key %q; want %s, launch, acme/widget", first.PRURL, first.DispatchReason, first.RepoKey, prURL)
	}

	h.resetWorker()
	fix, _ := h.launchRun("--pr", "12", "--reason", "ci-fix", "CI is failing on PR #12.")
	h.WaitForClaudeStart(30 * time.Second)
	fix = h.finishRun(fix.ID)
	if fix.ChangeID != first.ChangeID || fix.DispatchReason != event.DispatchCIFix {
		t.Fatalf("fix run: change %q, reason %q; want %q, ci-fix", fix.ChangeID, fix.DispatchReason, first.ChangeID)
	}

	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	wantSequence := "agent:started phase:completed/setup phase:started/agent check:started check:completed " +
		"phase:completed/agent phase:started/finalize agent:completed agent:pr-created phase:completed/finalize"
	for _, tc := range []struct {
		id, reason string
		keys       []string // the key each event carries, in order: the change until the run knows its PR
	}{
		{first.ID, "launch", []string{"acme/widget@" + first.ChangeID, "acme/widget#12"}},
		{fix.ID, "ci-fix", []string{"acme/widget#12"}},
	} {
		evts := h.runEvents(tc.id)
		var seq []string
		for _, e := range evts {
			s := e.Type
			if p, ok := e.Data["phase"].(string); ok {
				s += "/" + p
			}
			seq = append(seq, s)
		}
		if got := strings.Join(seq, " "); got != wantSequence {
			t.Errorf("run %s events:\n  %s\nwant:\n  %s", tc.id, got, wantSequence)
		}
		keyAt := 0
		for _, e := range evts {
			d := e.Data
			if d["repo_key"] != "acme/widget" || d["change_id"] != first.ChangeID || d["dispatch_reason"] != tc.reason {
				t.Errorf("run %s %s: repo_key %v, change_id %v, dispatch_reason %v", tc.id, e.Type, d["repo_key"], d["change_id"], d["dispatch_reason"])
			}
			if head, _ := d["head_sha"].(string); !sha.MatchString(head) {
				t.Errorf("run %s %s: head_sha %v", tc.id, e.Type, d["head_sha"])
			}
			if d["key"] != tc.keys[keyAt] && keyAt+1 < len(tc.keys) && d["key"] == tc.keys[keyAt+1] {
				keyAt++
			}
			if d["key"] != tc.keys[keyAt] {
				t.Errorf("run %s %s: key %v, want %s", tc.id, e.Type, d["key"], tc.keys[keyAt])
			}
			if e.Type == event.PhaseCompleted || e.Type == event.CheckCompleted {
				if ms, ok := d["duration_ms"].(float64); !ok || ms < 0 {
					t.Errorf("run %s %s: duration_ms %v", tc.id, e.Type, d["duration_ms"])
				}
			}
			if e.Type == event.CheckCompleted && (d["kind"] != "test" || d["command"] != "go test ./..." || d["success"] != true) {
				t.Errorf("run %s check:completed %+v", tc.id, d)
			}
		}
		if keyAt != len(tc.keys)-1 {
			t.Errorf("run %s: its events never carried key %s", tc.id, tc.keys[len(tc.keys)-1])
		}
	}

	res := h.RunKlaus("report", "time", "--current", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("report time exited %d: %s", res.ExitCode, res.Stderr)
	}
	var rep report.Report
	if err := json.Unmarshal([]byte(res.Stdout), &rep); err != nil {
		t.Fatalf("report JSON: %v\n%s", err, res.Stdout)
	}
	o := rep.Overall
	if o.Runs != 2 || o.Changes != 1 || o.FollowUps != 1 || o.FollowUpReasons["ci-fix"] != 1 ||
		o.Reasons["launch"] != 1 || o.Reasons["ci-fix"] != 1 || o.Unfinished != 0 || o.Untimed != 0 {
		t.Errorf("report overall %+v", o)
	}
	if c := o.Checks["test"]; c == nil || c.Count != 2 || c.Failed != 0 {
		t.Errorf("report test checks %+v", c)
	}
	if o.AgentMS <= 0 || o.CostRuns != 2 || fmt.Sprintf("%.2f", o.CostUSD) != "0.50" {
		t.Errorf("report agent %dms, cost $%v over %d runs", o.AgentMS, o.CostUSD, o.CostRuns)
	}
	if len(rep.Repos) != 1 || rep.Repos[0].Repo != "acme/widget" || len(rep.Changes) != 1 || rep.Changes[0].Key != "acme/widget#12" {
		t.Errorf("report repos %+v, changes %+v", rep.Repos, rep.Changes)
	}
}

// TestStreamChecksOutlastUnsavedState runs the real _format-stream as an
// agent's pane does (KLAUS_RUN_ID set) on a stream that runs go test and
// ends before launch has saved the run's state, as a fast agent's can. For
// one run launch saves the state soon after, and its check events carry the
// run's fields; for the other it never does, and its check events still land
// in the session's event log under the run's ID, where klaus report time
// counts them.
func TestStreamChecksOutlastUnsavedState(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	const agentStream = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":"ok"}]}}
`
	formatStream := func(id string) *exec.Cmd {
		cmd := exec.Command(klausBinary, "_format-stream")
		cmd.Dir = h.RepoDir
		cmd.Env = append(h.env(), "KLAUS_RUN_ID="+id)
		cmd.Stdin = strings.NewReader(agentStream)
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting _format-stream: %v", err)
		}
		return cmd
	}

	const savedLate, neverSaved = "20261021-0100-5a7ed1a7", "20261021-0100-0e7e5a7e"
	late, never := formatStream(savedLate), formatStream(neverSaved)
	time.Sleep(500 * time.Millisecond) // both streams have ended by now
	h.SeedState(&run.State{
		ID:             savedLate,
		CreatedAt:      time.Now().Format(time.RFC3339),
		RepoKey:        "acme/widget",
		ChangeID:       "Ifeedface",
		DispatchReason: event.DispatchLaunch,
	})
	for _, cmd := range []*exec.Cmd{late, never} {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("_format-stream: %v", err)
		}
	}

	for _, tc := range []struct {
		id      string
		repoKey interface{} // nil: the run's state never existed
	}{
		{savedLate, "acme/widget"},
		{neverSaved, nil},
	} {
		evts := h.runEvents(tc.id)
		var types []string
		for _, e := range evts {
			types = append(types, e.Type)
			if e.Data["kind"] != event.CheckTest || e.Data["command"] != "go test ./..." || e.Data["repo_key"] != tc.repoKey {
				t.Errorf("run %s %s: %+v", tc.id, e.Type, e.Data)
			}
			if e.Type == event.CheckCompleted && e.Data["success"] != true {
				t.Errorf("run %s check:completed: success %v", tc.id, e.Data["success"])
			}
		}
		if got, want := strings.Join(types, " "), "check:started check:completed"; got != want {
			t.Errorf("run %s events: %q, want %q", tc.id, got, want)
		}
	}

	res := h.RunKlaus("report", "time", "--current", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("report time exited %d: %s", res.ExitCode, res.Stderr)
	}
	var rep report.Report
	if err := json.Unmarshal([]byte(res.Stdout), &rep); err != nil {
		t.Fatalf("report JSON: %v\n%s", err, res.Stdout)
	}
	if c := rep.Overall.Checks["test"]; c == nil || c.Count != 2 || c.Failed != 0 {
		t.Errorf("report test checks %+v", c)
	}
}

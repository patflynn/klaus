package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/report"
)

// reportFixture writes two sessions' event logs under home and returns the
// second session's ID. Times are relative to now, so --since windows hold.
//
// Session A, new events: run r1 opens acme/widget PR 7 on change c1 (setup
// 30s, agent 1h with a test and a failing _pre-review, finalize 20s, $2.50);
// the pipeline's ci-fix run r2 starts 2h after r1 ends (setup 10s, agent
// 30m, finalize 10s, $1); the pipeline approves PR 7 29m40s after r2 ends.
// A review of PR 7 and two consults, one in acme/widget's checkout. Run r6
// is 10 days old.
//
// Session B: old runs r3 and r4 on PR 9, events without repo_key, change_id,
// dispatch_reason or phases (an hour of agent time, then 30m 9h later), and
// a docs-timer run r5 on a valley that hasn't finished.
func reportFixture(t *testing.T, home string) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	at := func(d time.Duration) time.Time { return now.Add(-d) }
	type ev struct {
		at   time.Time
		run  string
		typ  string
		data map[string]interface{}
	}
	nano := func(t time.Time) string { return t.Format(time.RFC3339Nano) }
	phase := func(run, name string, start time.Time, d time.Duration, extra map[string]interface{}) ev {
		data := map[string]interface{}{"phase": name, "started_at": nano(start), "ended_at": nano(start.Add(d)), "duration_ms": d.Milliseconds()}
		for k, v := range extra {
			data[k] = v
		}
		return ev{start.Add(d), run, event.PhaseCompleted, data}
	}
	keys := func(reason, repo, change, pr string, extra map[string]interface{}) map[string]interface{} {
		data := map[string]interface{}{"dispatch_reason": reason, "repo_key": repo, "change_id": change}
		if pr != "" {
			data["pr_number"], data["key"] = pr, repo+"#"+pr
		} else {
			data["key"] = repo + "@" + change
		}
		for k, v := range extra {
			data[k] = v
		}
		return data
	}

	r1Setup := at(50 * time.Hour)
	r1Agent := r1Setup.Add(30 * time.Second)
	r1End := r1Agent.Add(time.Hour)
	r2Setup := r1End.Add(20 * time.Second).Add(2 * time.Hour)
	r2Agent := r2Setup.Add(10 * time.Second)
	r2End := r2Agent.Add(30 * time.Minute)
	r6Setup := at(240 * time.Hour)
	r5Setup := at(2 * time.Hour)

	sessionA := []ev{
		phase("r6", event.PhaseSetup, r6Setup, 5*time.Second, keys("launch", "acme/old", "c6", "", nil)),
		{r6Setup.Add(5 * time.Second), "r6", event.AgentStarted, keys("launch", "acme/old", "c6", "", nil)},
		phase("r6", event.PhaseAgent, r6Setup.Add(5*time.Second), time.Hour, keys("launch", "acme/old", "c6", "", nil)),

		phase("r1", event.PhaseSetup, r1Setup, 30*time.Second, keys("launch", "acme/widget", "c1", "", nil)),
		{r1Agent, "r1", event.AgentStarted, keys("launch", "acme/widget", "c1", "", map[string]interface{}{"target_repo": "widget"})},
		{r1Agent.Add(10 * time.Minute), "r1", event.CheckCompleted, keys("launch", "acme/widget", "c1", "", map[string]interface{}{"kind": "test", "duration_ms": 60000, "success": true})},
		{r1Agent.Add(50 * time.Minute), "r1", event.CheckCompleted, keys("launch", "acme/widget", "c1", "", map[string]interface{}{"kind": "pre-review", "duration_ms": 90000, "success": false})},
		phase("r1", event.PhaseAgent, r1Agent, time.Hour, keys("launch", "acme/widget", "c1", "7", map[string]interface{}{"cost_usd": 2.5})),
		{r1End, "r1", event.AgentCompleted, keys("launch", "acme/widget", "c1", "7", map[string]interface{}{"cost_usd": 2.5})},
		{r1End, "r1", event.AgentPRCreated, keys("launch", "acme/widget", "c1", "7", map[string]interface{}{"pr_url": "https://github.com/acme/widget/pull/7"})},
		phase("r1", event.PhaseFinalize, r1End, 20*time.Second, keys("launch", "acme/widget", "c1", "7", nil)),
		{r1End.Add(time.Hour), "7", event.AgentCIFailed, map[string]interface{}{"pr_number": "7", "pr_url": "https://github.com/acme/widget/pull/7"}},
		{at(48 * time.Hour), "consult-1", event.ConsultCompleted, map[string]interface{}{"repo_key": "acme/widget", "duration_ms": 60000, "success": true}},
		{at(47 * time.Hour), "consult-2", event.ConsultCompleted, map[string]interface{}{"duration_ms": 30000, "success": true}},

		phase("r2", event.PhaseSetup, r2Setup, 10*time.Second, keys("ci-fix", "acme/widget", "c1", "7", nil)),
		{r2Agent, "r2", event.AgentStarted, keys("ci-fix", "acme/widget", "c1", "7", nil)},
		phase("r2", event.PhaseAgent, r2Agent, 30*time.Minute, keys("ci-fix", "acme/widget", "c1", "7", map[string]interface{}{"cost_usd": 1.0})),
		{r2End, "r2", event.AgentCompleted, keys("ci-fix", "acme/widget", "c1", "7", map[string]interface{}{"cost_usd": 1.0})},
		phase("r2", event.PhaseFinalize, r2End, 10*time.Second, keys("ci-fix", "acme/widget", "c1", "7", nil)),
		{r2End.Add(10 * time.Second).Add(29*time.Minute + 40*time.Second), "7", event.PRApproved, map[string]interface{}{"pr_number": "7", "pr_url": "https://github.com/acme/widget/pull/7"}},
		{at(46 * time.Hour), "review-1", event.ReviewCompleted, map[string]interface{}{"repo": "acme/widget", "pr_number": "7", "duration_ms": 120000, "success": true}},
	}
	sessionB := []ev{
		{at(30 * time.Hour), "r3", event.AgentStarted, map[string]interface{}{"id": "r3", "prompt": "fix the parser", "target_repo": "widget"}},
		{at(29 * time.Hour), "r3", event.AgentCompleted, map[string]interface{}{"id": "r3", "cost_usd": 0.5, "duration_ms": 3500000}},
		{at(29 * time.Hour), "r3", event.AgentPRCreated, map[string]interface{}{"id": "r3", "pr_url": "https://github.com/acme/widget/pull/9", "pr_number": "9"}},
		{at(20 * time.Hour), "r4", event.AgentStarted, map[string]interface{}{"id": "r4", "prompt": "address the review", "target_repo": "widget"}},
		{at(19*time.Hour + 30*time.Minute), "r4", event.AgentCompleted, map[string]interface{}{"id": "r4", "cost_usd": 0.25}},
		{at(19*time.Hour + 30*time.Minute), "r4", event.AgentPRCreated, map[string]interface{}{"id": "r4", "pr_url": "https://github.com/acme/widget/pull/9", "pr_number": "9"}},
		phase("r5", event.PhaseSetup, r5Setup, 5*time.Second, keys("docs-timer", "valley:gadget", "c5", "", nil)),
		{r5Setup.Add(5 * time.Second), "r5", event.AgentStarted, keys("docs-timer", "valley:gadget", "c5", "", nil)},
		{r5Setup.Add(5 * time.Second), "r5", event.PhaseStarted, keys("docs-timer", "valley:gadget", "c5", "", map[string]interface{}{"phase": "agent"})},
	}

	write := func(session string, evts []ev) {
		dir := filepath.Join(home, ".klaus", "sessions", session)
		log := event.NewLog(dir)
		for _, e := range evts {
			if err := log.Emit(event.Event{Timestamp: e.at.Format(time.RFC3339), RunID: e.run, Type: e.typ, Data: e.data}); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("session-20261018-0900-aaaa", sessionA)
	write("session-20261019-0900-bbbb", sessionB)
	return "session-20261019-0900-bbbb"
}

func runReport(t *testing.T, bin, home, session string, args ...string) string {
	t.Helper()
	c := exec.Command(bin, append([]string{"report", "time"}, args...)...)
	c.Env = append(os.Environ(), "HOME="+home, sessionIDEnv+"="+session)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("klaus report time %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func reportJSON(t *testing.T, bin, home, session string, args ...string) report.Report {
	t.Helper()
	out := runReport(t, bin, home, session, append(args, "--json")...)
	var rep report.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("report JSON: %v\n%s", err, out)
	}
	return rep
}

// TestReportTimeSumsEventLogs runs the real klaus report time over the
// fixture's two sessions.
func TestReportTimeSumsEventLogs(t *testing.T) {
	bin := klausBinary(t)
	home := t.TempDir()
	current := reportFixture(t, home)

	t.Run("a week of every session", func(t *testing.T) {
		rep := reportJSON(t, bin, home, current)
		o := rep.Overall
		if rep.Sessions != 2 || o.Runs != 5 || o.Unfinished != 1 || o.Untimed != 2 {
			t.Errorf("sessions %d, runs %d, unfinished %d, untimed %d; want 2, 5 (r6 is older), 1 (r5), 2 (r3, r4)", rep.Sessions, o.Runs, o.Unfinished, o.Untimed)
		}
		wantReasons := map[string]int{"launch": 1, "ci-fix": 1, "unknown": 2, "docs-timer": 1}
		if !sameCounts(o.Reasons, wantReasons) {
			t.Errorf("reasons %v, want %v", o.Reasons, wantReasons)
		}
		// r3 and r4 have no phases: agent:started to their last event.
		if o.SetupMS != 45000 || o.AgentMS != (3*time.Hour).Milliseconds() || o.FinalizeMS != 30000 {
			t.Errorf("setup %d, agent %d, finalize %d; want 45000, %d, 30000", o.SetupMS, o.AgentMS, (3 * time.Hour).Milliseconds(), o.FinalizeMS)
		}
		if c := o.Checks["test"]; c == nil || c.Count != 1 || c.MS != 60000 || c.Failed != 0 {
			t.Errorf("test checks %+v", c)
		}
		if c := o.Checks["pre-review"]; c == nil || c.Count != 1 || c.MS != 90000 || c.Failed != 1 {
			t.Errorf("pre-review checks %+v", c)
		}
		// r2 follows r1 on change c1, r4 follows r3 on PR 9; r5's docs-timer
		// run starts its change.
		if o.Changes != 3 || o.FollowUps != 2 || !sameCounts(o.FollowUpReasons, map[string]int{"ci-fix": 1, "unknown": 1}) {
			t.Errorf("changes %d, follow-ups %d %v; want 3, 2 {ci-fix, unknown}", o.Changes, o.FollowUps, o.FollowUpReasons)
		}
		wantWait := (2*time.Hour + 9*time.Hour).Milliseconds()
		if o.WaitMS != wantWait {
			t.Errorf("wait between runs %d, want %d", o.WaitMS, wantWait)
		}
		if o.Approvals != 1 || o.ApprovalWaitMS != (29*time.Minute+40*time.Second).Milliseconds() {
			t.Errorf("approvals %d after %dms; want 1 after 29m40s", o.Approvals, o.ApprovalWaitMS)
		}
		if o.Consults != 2 || o.ConsultMS != 90000 || o.Reviews != 1 || o.ReviewMS != 120000 {
			t.Errorf("consults %d %dms, reviews %d %dms", o.Consults, o.ConsultMS, o.Reviews, o.ReviewMS)
		}
		if o.CostRuns != 4 || o.CostUSD != 4.25 {
			t.Errorf("cost $%v over %d runs, want $4.25 over 4", o.CostUSD, o.CostRuns)
		}

		repos := map[string]report.Totals{}
		for _, r := range rep.Repos {
			repos[r.Repo] = r
		}
		// r3 and r4 named their repo "widget" until their PR said acme/widget.
		if w := repos["acme/widget"]; w.Runs != 4 || w.Consults != 1 || w.Reviews != 1 || w.FollowUps != 2 || w.Approvals != 1 {
			t.Errorf("acme/widget %+v", w)
		}
		if g := repos["valley:gadget"]; g.Runs != 1 || g.Unfinished != 1 || g.Reasons["docs-timer"] != 1 {
			t.Errorf("valley:gadget %+v", g)
		}
		if len(rep.Repos) != 2 {
			t.Errorf("repos %v, want acme/widget and valley:gadget", repos)
		}

		changes := map[string]report.Change{}
		for _, c := range rep.Changes {
			changes[c.Key] = c
		}
		c1 := changes["acme/widget#7"]
		if c1.Runs != 2 || len(c1.FollowUps) != 1 || c1.FollowUps[0].RunID != "r2" || c1.FollowUps[0].Reason != "ci-fix" ||
			c1.FollowUps[0].WaitMS != (2*time.Hour).Milliseconds() {
			t.Errorf("change acme/widget#7 %+v", c1)
		}
		if c := changes["acme/widget#9"]; c.Runs != 2 || len(c.FollowUps) != 1 || c.FollowUps[0].Reason != "unknown" {
			t.Errorf("change acme/widget#9 %+v", c)
		}
		if c := changes["valley:gadget@c5"]; c.Runs != 1 || len(c.FollowUps) != 0 {
			t.Errorf("change valley:gadget@c5 %+v", c)
		}
	})

	t.Run("all time", func(t *testing.T) {
		rep := reportJSON(t, bin, home, current, "--since", "0")
		if rep.Overall.Runs != 6 || rep.Since != "" {
			t.Errorf("runs %d since %q, want 6 (r6 too) since all time", rep.Overall.Runs, rep.Since)
		}
	})

	t.Run("one repo", func(t *testing.T) {
		rep := reportJSON(t, bin, home, current, "--repo", "widget")
		o := rep.Overall
		if o.Runs != 4 || o.Consults != 1 || o.Reviews != 1 || len(rep.Repos) != 1 || rep.Repos[0].Repo != "acme/widget" {
			t.Errorf("--repo widget: runs %d, consults %d, reviews %d, repos %+v", o.Runs, o.Consults, o.Reviews, rep.Repos)
		}
	})

	t.Run("the current session", func(t *testing.T) {
		rep := reportJSON(t, bin, home, current, "--current")
		if rep.Sessions != 1 || rep.Overall.Runs != 3 || rep.Overall.Consults != 0 {
			t.Errorf("--current: sessions %d, runs %d, consults %d; want 1, 3, 0", rep.Sessions, rep.Overall.Runs, rep.Overall.Consults)
		}
	})

	t.Run("table", func(t *testing.T) {
		out := runReport(t, bin, home, current)
		for _, want := range []string{
			"REPO", "acme/widget", "valley:gadget", "TOTAL",
			"DISPATCH REASON", "docs-timer", "ci-fix",
			"CHECK", "pre-review",
			"acme/widget#7", "acme/widget#9",
			"Cost is known for 4 of 5 runs.", "2 run(s) predate phase events", "1 run(s) have not finished.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("table missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("bad --since", func(t *testing.T) {
		c := exec.Command(bin, "report", "time", "--since", "a week")
		c.Env = append(os.Environ(), "HOME="+home)
		out, err := c.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "use a duration such as 7d") {
			t.Errorf("--since 'a week' should fail naming the forms: %v\n%s", err, out)
		}
	})
}

func sameCounts(got, want map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

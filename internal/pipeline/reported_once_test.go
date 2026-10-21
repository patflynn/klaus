package pipeline

import (
	"context"
	"fmt"
	"testing"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
)

func prURL(n string) string { return "https://github.com/patflynn/klaus/pull/" + n }

// prEvents returns the pr:approved and pr:merged events in the session's log.
func prEvents(t *testing.T, dir string) []event.Event {
	t.Helper()
	var out []event.Event
	for _, e := range readEvents(t, dir) {
		if e.Type == event.PRApproved || e.Type == event.PRMerged {
			out = append(out, e)
		}
	}
	return out
}

// TestLongMergedPRsAreNotReReported replays the 2026-10-21 incident: run
// states for PRs merged days earlier, approved through klaus, polled every
// pass. When GitHub briefly couldn't be asked for their state (UNKNOWN) while
// the other queries still answered, the pipeline took them for open, tracked
// them afresh, reported them approved, tried to merge them, and reported them
// merged again once the next poll got through. None of that may happen, on
// any pass or after a restart.
func TestLongMergedPRsAreNotReReported(t *testing.T) {
	dir := t.TempDir()
	approved := true
	var runStates []*run.State
	for _, n := range []string{"329", "330", "331"} {
		u := prURL(n)
		runStates = append(runStates, &run.State{ID: "run-" + n, PRURL: &u, Approved: &approved})
	}
	statuses := func(state string) map[string]*PRStatus {
		out := make(map[string]*PRStatus)
		for _, n := range []string{"329", "330", "331"} {
			s := &PRStatus{PRNumber: n, PRURL: prURL(n), State: state, TargetRepo: "klaus"}
			if state == "UNKNOWN" {
				s.CI, s.Conflicts = "passing", "none"
			}
			out[n] = s
		}
		return out
	}

	for restart := 0; restart < 2; restart++ {
		c := controllerIn(t, dir)
		c.SetAutoMergeOnApproval(true)
		merges := 0
		c.SetMergePRs(func(context.Context, string, []string) error {
			merges++
			return nil
		})

		for _, state := range []string{"MERGED", "UNKNOWN", "MERGED", "UNKNOWN", "UNKNOWN", "MERGED"} {
			c.HandleGHStatus(context.Background(), statuses(state), runStates)
			if n := len(c.PipelineStates()); n != 0 {
				t.Fatalf("restart %d, %s pass: %d long-merged PRs tracked, want none", restart, state, n)
			}
		}
		if merges != 0 {
			t.Errorf("restart %d: %d merge attempts on long-merged PRs", restart, merges)
		}
	}
	if evts := readEvents(t, dir); len(evts) != 0 {
		t.Errorf("long-merged PRs emitted %d events, want none: %+v", len(evts), evts)
	}
}

// TestNewTransitionsAreReportedOnce: a PR klaus sees open, then approved,
// then merged, reports each once, through UNKNOWN passes, repeated polls and
// restarts.
func TestNewTransitionsAreReportedOnce(t *testing.T) {
	dir := t.TempDir()
	status := &PRStatus{PRNumber: "336", PRURL: prURL("336"), State: "OPEN", CI: "pending", TargetRepo: "klaus"}
	statuses := map[string]*PRStatus{"336": status}
	poll := func(c *Controller, state string) {
		status.State = state
		c.HandleGHStatus(context.Background(), statuses, nil)
	}

	c := controllerIn(t, dir)
	poll(c, "OPEN")
	status.CI, status.Conflicts, status.ReviewDecision = "passing", "none", "APPROVED"
	poll(c, "OPEN")
	poll(c, "UNKNOWN")
	poll(c, "OPEN")

	c = controllerIn(t, dir)
	poll(c, "OPEN")
	poll(c, "MERGED")
	poll(c, "UNKNOWN")
	poll(c, "MERGED")

	c = controllerIn(t, dir)
	poll(c, "MERGED")
	poll(c, "UNKNOWN")
	poll(c, "MERGED")

	evts := prEvents(t, dir)
	var got []string
	for _, e := range evts {
		got = append(got, fmt.Sprintf("%s %v", e.Type, e.Data["pr_url"]))
	}
	want := []string{"pr:approved " + prURL("336"), "pr:merged " + prURL("336")}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// TestMergeWhileDashboardDownIsReported: klaus saw the PR open, the dashboard
// was down when it merged, so the restarted dashboard's first look at it is
// MERGED. That merge was never reported; it is, once.
func TestMergeWhileDashboardDownIsReported(t *testing.T) {
	dir := t.TempDir()
	statuses := map[string]*PRStatus{
		"337": {PRNumber: "337", PRURL: prURL("337"), State: "OPEN", CI: "pending", TargetRepo: "klaus"},
	}
	controllerIn(t, dir).HandleGHStatus(context.Background(), statuses, nil)

	statuses["337"] = &PRStatus{PRNumber: "337", PRURL: prURL("337"), State: "MERGED"}
	for restart := 0; restart < 2; restart++ {
		controllerIn(t, dir).HandleGHStatus(context.Background(), statuses, nil)
	}

	if got := countEvents(prEvents(t, dir), event.PRMerged); got != 1 {
		t.Errorf("pr:merged emitted %d times, want 1", got)
	}
}

// TestAutoMergedPRIsReportedOnce: a merge klaus made itself is reported when
// the merge returns, and not again when the next poll sees it merged.
func TestAutoMergedPRIsReportedOnce(t *testing.T) {
	dir := t.TempDir()
	c := controllerIn(t, dir)
	c.SetAutoMergeOnApproval(true)
	c.SetMergePRs(func(context.Context, string, []string) error { return nil })

	statuses := map[string]*PRStatus{
		"338": {PRNumber: "338", PRURL: prURL("338"), State: "OPEN", CI: "passing",
			Conflicts: "none", ReviewDecision: "APPROVED", TargetRepo: "klaus"},
	}
	c.HandleGHStatus(context.Background(), statuses, nil)
	statuses["338"] = &PRStatus{PRNumber: "338", PRURL: prURL("338"), State: "MERGED"}
	c.HandleGHStatus(context.Background(), statuses, nil)
	controllerIn(t, dir).HandleGHStatus(context.Background(), statuses, nil)

	var merged []event.Event
	for _, e := range prEvents(t, dir) {
		if e.Type == event.PRMerged {
			merged = append(merged, e)
		}
	}
	if len(merged) != 1 {
		t.Fatalf("pr:merged emitted %d times, want 1", len(merged))
	}
	if got := merged[0].Data["pr_url"]; got != prURL("338") {
		t.Errorf("pr:merged pr_url = %v, want %s", got, prURL("338"))
	}
}

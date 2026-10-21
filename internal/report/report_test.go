package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
)

var day = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

// at is a time on the test's day, such as "10:30".
func at(t *testing.T, hhmm string) time.Time {
	t.Helper()
	d, err := time.Parse("15:04", hhmm)
	if err != nil {
		t.Fatal(err)
	}
	return day.Add(time.Duration(d.Hour())*time.Hour + time.Duration(d.Minute())*time.Minute)
}

func ev(ts time.Time, runID, typ string, data map[string]interface{}) event.Event {
	return event.Event{Timestamp: ts.Format(time.RFC3339), RunID: runID, Type: typ, Data: data}
}

// fields are a run event's keys: repo_key, change_id, pr_number when pr
// isn't "", and dispatch_reason.
func fields(repo, change, pr, reason string) map[string]interface{} {
	f := map[string]interface{}{"repo_key": repo, "change_id": change, "dispatch_reason": reason}
	if pr != "" {
		f["pr_number"] = pr
	}
	return f
}

// newRun is a run's events in the current format: agent:started at start
// and, unless end is zero, the finalize phase ending at end.
func newRun(id string, start, end time.Time, f map[string]interface{}) []event.Event {
	evts := []event.Event{ev(start, id, event.AgentStarted, f)}
	if !end.IsZero() {
		data := map[string]interface{}{"phase": event.PhaseFinalize, "started_at": end.Format(time.RFC3339Nano), "ended_at": end.Format(time.RFC3339Nano), "duration_ms": 0}
		for k, v := range f {
			data[k] = v
		}
		evts = append(evts, ev(end, id, event.PhaseCompleted, data))
	}
	return evts
}

// build reports on one session's events, read back as from its log.
func build(t *testing.T, evts ...[]event.Event) *Report {
	t.Helper()
	var all []event.Event
	for _, e := range evts {
		all = append(all, e...)
	}
	b, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	var read []event.Event
	if err := json.Unmarshal(b, &read); err != nil {
		t.Fatal(err)
	}
	return Build([][]event.Event{read}, Options{})
}

func changesByKey(rep *Report) map[string]Change {
	m := map[string]Change{}
	for _, c := range rep.Changes {
		m[c.Key] = c
	}
	return m
}

// TestWaitIsBetweenTheChangesRuns runs four runs on one change: A 10:00 to
// 12:00, B 10:30 to 11:00 within it, C 11:30 to 12:30, while A still runs,
// and D from 13:00, which never ends; then E at 14:00. The change waits only
// from 12:30, when A and C have both ended, to D, and how long it waited for
// E is unknown.
func TestWaitIsBetweenTheChangesRuns(t *testing.T) {
	f := fields("acme/widget", "c1", "", event.DispatchLaunch)
	rep := build(t,
		newRun("A", at(t, "10:00"), at(t, "12:00"), f),
		newRun("B", at(t, "10:30"), at(t, "11:00"), f),
		newRun("C", at(t, "11:30"), at(t, "12:30"), f),
		newRun("D", at(t, "13:00"), time.Time{}, f),
		newRun("E", at(t, "14:00"), at(t, "15:00"), f),
	)
	if len(rep.Changes) != 1 {
		t.Fatalf("changes %+v, want one", rep.Changes)
	}
	var waits []int64
	var unknown []bool
	for _, fu := range rep.Changes[0].FollowUps {
		waits = append(waits, fu.WaitMS)
		unknown = append(unknown, fu.WaitUnknown)
	}
	wantWaits, wantUnknown := []int64{0, 0, (30 * time.Minute).Milliseconds(), 0}, []bool{false, false, false, true}
	for i := range wantWaits {
		if len(waits) != len(wantWaits) || waits[i] != wantWaits[i] || unknown[i] != wantUnknown[i] {
			t.Fatalf("follow-up waits %v unknown %v, want %v unknown %v", waits, unknown, wantWaits, wantUnknown)
		}
	}
	if o := rep.Overall; o.WaitMS != (30*time.Minute).Milliseconds() || o.UnknownWaits != 1 {
		t.Errorf("overall wait %dms, unknown %d; want 30m, 1", o.WaitMS, o.UnknownWaits)
	}
	var text strings.Builder
	if err := rep.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if want := "1 follow-up(s) started after a run with no end on their change: their wait is unknown."; !strings.Contains(text.String(), want) {
		t.Errorf("table should say %q:\n%s", want, text.String())
	}
}

// TestChangeIdentities groups runs whose events name their change in
// different ways:
//
//   - PR 9: an old run that knows only its PR (named "widget" until its PR
//     URL says acme/widget), and a ci-fix run with the PR and a change ID,
//     which the pipeline then approves
//   - change "dup": a Change-Id copied into acme/widget and acme/gizmo
//   - change c10 on a renamed repo: the first run starts as acme/gadget-old
//     and finalizes as acme/gadget with PR 3, a follow-up still says
//     acme/gadget-old, and the approval says ACME/Gadget
func TestChangeIdentities(t *testing.T) {
	old := []event.Event{
		ev(at(t, "08:00"), "r3", event.AgentStarted, map[string]interface{}{"id": "r3", "target_repo": "widget"}),
		ev(at(t, "09:00"), "r3", event.AgentCompleted, map[string]interface{}{"id": "r3"}),
		ev(at(t, "09:00"), "r3", event.AgentPRCreated, map[string]interface{}{"id": "r3", "pr_url": "https://github.com/acme/widget/pull/9", "pr_number": "9"}),
	}
	approve := func(ts time.Time, url, pr string) []event.Event {
		return []event.Event{ev(ts, pr, event.PRApproved, map[string]interface{}{"pr_url": url, "pr_number": pr})}
	}
	renamed := newRun("n1", at(t, "10:00"), time.Time{}, fields("acme/gadget-old", "c10", "", event.DispatchLaunch))
	renamed = append(renamed, newRun("n1", at(t, "10:00"), at(t, "10:30"), fields("acme/gadget", "c10", "3", event.DispatchLaunch))[1:]...)
	rep := build(t,
		old,
		newRun("r7", at(t, "11:00"), at(t, "11:30"), fields("acme/widget", "c9", "9", event.DispatchCIFix)),
		approve(at(t, "11:40"), "https://github.com/acme/widget/pull/9", "9"),
		newRun("g1", at(t, "08:00"), at(t, "08:30"), fields("acme/widget", "dup", "", event.DispatchLaunch)),
		newRun("g2", at(t, "08:00"), at(t, "08:30"), fields("acme/gizmo", "dup", "", event.DispatchLaunch)),
		renamed,
		newRun("n2", at(t, "11:00"), at(t, "11:20"), fields("acme/gadget-old", "c10", "", event.DispatchChangeFollowUp)),
		approve(at(t, "12:00"), "https://github.com/ACME/Gadget/pull/3", "3"),
	)

	changes := changesByKey(rep)
	if len(rep.Changes) != 4 {
		t.Errorf("changes %+v, want acme/widget#9, acme/widget@dup, acme/gizmo@dup and acme/gadget#3", rep.Changes)
	}
	if c := changes["acme/widget#9"]; c.Runs != 2 || len(c.FollowUps) != 1 || c.FollowUps[0].RunID != "r7" ||
		c.WaitMS != (2*time.Hour).Milliseconds() || c.ApprovalWaitMS != (10*time.Minute).Milliseconds() {
		t.Errorf("acme/widget#9 %+v, want r3 then r7 2h later, approved 10m after", c)
	}
	for _, k := range []string{"acme/widget@dup", "acme/gizmo@dup"} {
		if c := changes[k]; c.Runs != 1 || len(c.FollowUps) != 0 {
			t.Errorf("%s %+v, want its own run only", k, c)
		}
	}
	if c := changes["acme/gadget#3"]; c.Runs != 2 || len(c.FollowUps) != 1 || c.FollowUps[0].RunID != "n2" ||
		c.ApprovalWaitMS != (40*time.Minute).Milliseconds() {
		t.Errorf("acme/gadget#3 %+v, want n1 then n2, approved 40m after n2", c)
	}
	if o := rep.Overall; o.Changes != 4 || o.FollowUps != 2 || o.Approvals != 2 {
		t.Errorf("overall changes %d, follow-ups %d, approvals %d; want 4, 2, 2", o.Changes, o.FollowUps, o.Approvals)
	}
}

// TestCheckIncludingPreReview runs a command line of go test and
// _pre-review from 10:00 to 10:05, whose pre-review reports itself from
// 10:01, and a lint alongside it from 10:02 to 10:03. The test check keeps
// only the minute before the pre-review; the lint, which ran no pre-review,
// keeps its own.
func TestCheckIncludingPreReview(t *testing.T) {
	f := fields("acme/widget", "c1", "", event.DispatchLaunch)
	check := func(kind string, start, end time.Time, extra map[string]interface{}) event.Event {
		data := map[string]interface{}{"kind": kind, "started_at": start.Format(time.RFC3339Nano), "ended_at": end.Format(time.RFC3339Nano), "duration_ms": end.Sub(start).Milliseconds()}
		for k, v := range extra {
			data[k] = v
		}
		return ev(end, "r1", event.CheckCompleted, data)
	}
	rep := build(t,
		newRun("r1", at(t, "09:50"), at(t, "10:10"), f),
		[]event.Event{
			check(event.CheckLint, at(t, "10:02"), at(t, "10:03"), nil),
			check(event.CheckPreReview, at(t, "10:01"), at(t, "10:05"), map[string]interface{}{"source": "pre-review"}),
			check(event.CheckTest, at(t, "10:00"), at(t, "10:05"), map[string]interface{}{"source": "stream", "includes_pre_review": true}),
		},
	)
	checks := rep.Overall.Checks
	for kind, want := range map[string]time.Duration{event.CheckTest: time.Minute, event.CheckPreReview: 4 * time.Minute, event.CheckLint: time.Minute} {
		if c := checks[kind]; c == nil || c.Count != 1 || c.MS != want.Milliseconds() {
			t.Errorf("%s checks %+v, want one of %v", kind, c, want)
		}
	}
}

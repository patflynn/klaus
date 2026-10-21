// Package report sums klaus's session event logs into where agent wall-clock
// time goes: per repository and overall, the runs and their phases, the
// checks agents ran, the wait between runs on the same change, follow-up runs
// and their reasons, consults, reviews and cost.
package report

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/event"
)

// Options narrow a report.
type Options struct {
	Since time.Time // runs, consults and reviews that started before it are left out; zero keeps all
	Until time.Time // the report's end, for its header; zero is unset
	Repo  string    // keep only this repo: a repo key, or its name after valley: or its last path component
}

// Report is where agent time went.
type Report struct {
	Since    string   `json:"since,omitempty"` // RFC3339; empty for all time
	Until    string   `json:"until,omitempty"`
	Sessions int      `json:"sessions"` // event logs read
	Overall  Totals   `json:"overall"`
	Repos    []Totals `json:"repos"`   // by repo key; "" for runs whose repo no event names
	Changes  []Change `json:"changes"` // every change with a run in the report, most runs first
}

// Totals sum a repo's runs, or every run. Durations are milliseconds.
type Totals struct {
	Repo string `json:"repo,omitempty"`
	Runs int    `json:"runs"`
	// Unfinished runs have no end in the logs: still running, or killed
	// before finalize.
	Unfinished int `json:"unfinished,omitempty"`
	// Reasons counts runs by dispatch reason; "unknown" for runs from
	// before klaus recorded one.
	Reasons    map[string]int `json:"reasons"`
	SetupMS    int64          `json:"setup_ms"`
	AgentMS    int64          `json:"agent_ms"`
	FinalizeMS int64          `json:"finalize_ms"`
	// Untimed runs predate phase events: their agent time is the span from
	// agent:started to the event that ended them, and they have no setup or
	// finalize time.
	Untimed int `json:"untimed,omitempty"`
	// Checks sums check:completed by kind. Checks run within the agent
	// phase, so their time is part of AgentMS.
	Checks map[string]*CheckTotals `json:"checks"`
	// Changes counts the changes the runs worked on, and FollowUps the
	// runs that continued one: a run after the change's first in the
	// report, or one dispatched as a follow-up (a fix, --pr, --change,
	// --resume-from), whose first run is earlier.
	Changes         int            `json:"changes"`
	FollowUps       int            `json:"follow_ups"`
	FollowUpReasons map[string]int `json:"follow_up_reasons"`
	// WaitMS sums the time a change waited between one run ending and the
	// next starting: on review, CI or the operator.
	WaitMS int64 `json:"wait_between_runs_ms"`
	// ApprovalWaitMS sums, over Approvals changes, the time from the run
	// that ended last before a change's first approval to that approval.
	ApprovalWaitMS int64   `json:"approval_wait_ms"`
	Approvals      int     `json:"approvals"`
	Consults       int     `json:"consults"`
	ConsultMS      int64   `json:"consult_ms"`
	Reviews        int     `json:"reviews"`
	ReviewMS       int64   `json:"review_ms"`
	CostUSD        float64 `json:"cost_usd"`
	CostRuns       int     `json:"cost_runs"` // runs whose cost is known
}

// CheckTotals sum one kind of check.
type CheckTotals struct {
	Count  int   `json:"count"`
	MS     int64 `json:"ms"`
	Failed int   `json:"failed"`
}

// Change is one change's runs.
type Change struct {
	Key            string     `json:"key"` // <repo>#<pr>, <repo>@<change id>, or the run ID of a lone run
	Repo           string     `json:"repo,omitempty"`
	Runs           int        `json:"runs"`
	AgentMS        int64      `json:"agent_ms"`
	WaitMS         int64      `json:"wait_between_runs_ms"`
	ApprovalWaitMS int64      `json:"approval_wait_ms,omitempty"`
	FollowUps      []FollowUp `json:"follow_ups"`
}

// FollowUp is a run that continued a change.
type FollowUp struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason"`
	WaitMS int64  `json:"wait_ms"` // since the change's previous run in the report ended; 0 when unknown
}

// unknownReason is a run's reason when its events don't say.
const unknownReason = "unknown"

// followUpReasons are the reasons that continue an existing change.
var followUpReasons = map[string]bool{
	event.DispatchPRFollowUp:       true,
	event.DispatchChangeFollowUp:   true,
	event.DispatchResume:           true,
	event.DispatchBudgetResume:     true,
	event.DispatchCIFix:            true,
	event.DispatchReviewFix:        true,
	event.DispatchReviewCommentFix: true,
	event.DispatchConflictFix:      true,
}

// runInfo is what one run's events say about it.
type runInfo struct {
	id                  string
	repo, changeID, pr  string
	reason              string
	start, end          time.Time
	agentStartedAt      time.Time
	terminalAt          time.Time
	setup, agent, final int64
	timed               bool // has phase:completed for agent
	cost                *float64
	checks              []check
	finished            bool
}

type check struct {
	kind    string
	ms      int64
	success *bool
}

// approval is an approval of a PR, from the pipeline or klaus approve.
type approval struct {
	key string
	at  time.Time
}

// job is a consult or review that finished.
type job struct {
	review bool
	repo   string
	start  time.Time
	ms     int64
}

// Build sums sessions' event logs, one slice of events per session.
func Build(sessions [][]event.Event, opts Options) *Report {
	runs := map[string]*runInfo{}
	var order []string
	var approvals []approval
	var jobs []job

	for _, evts := range sessions {
		for _, e := range evts {
			at, err := time.Parse(time.RFC3339, e.Timestamp)
			if err != nil {
				continue
			}
			d := data(e.Data)
			switch e.Type {
			case event.ConsultCompleted, event.ReviewCompleted:
				ms := d.int("duration_ms")
				repo := d.str("repo_key")
				if e.Type == event.ReviewCompleted && repo == "" {
					repo = d.str("repo")
				}
				jobs = append(jobs, job{review: e.Type == event.ReviewCompleted, repo: repo, start: at.Add(-time.Duration(ms) * time.Millisecond), ms: ms})
				continue
			case event.PRApproved, event.PRApprovalChanged:
				if e.Type == event.PRApprovalChanged && d.str("approved") != "true" {
					continue
				}
				if key := prKey(d, e.RunID); key != "" {
					approvals = append(approvals, approval{key: key, at: at})
				}
				continue
			}
			if !isRunID(e.RunID) {
				continue // a PR's pipeline event, or a consult's or review's start
			}
			r := runs[e.RunID]
			if r == nil {
				r = &runInfo{id: e.RunID}
				runs[e.RunID] = r
				order = append(order, e.RunID)
			}
			r.observe(e.Type, at, d)
		}
	}

	rep := &Report{Sessions: len(sessions)}
	if !opts.Since.IsZero() {
		rep.Since = opts.Since.UTC().Format(time.RFC3339)
	}
	if !opts.Until.IsZero() {
		rep.Until = opts.Until.UTC().Format(time.RFC3339)
	}

	// Runs from before repo_key name their repo by its short name; one that
	// newer events qualify one way only takes that key.
	qualified := map[string]map[string]bool{}
	for _, r := range runs {
		r.finish()
		if isQualified(r.repo) {
			name := repoName(r.repo)
			if qualified[name] == nil {
				qualified[name] = map[string]bool{}
			}
			qualified[name][r.repo] = true
		}
	}
	qualify := func(repo string) string {
		if keys := qualified[repo]; !isQualified(repo) && len(keys) == 1 {
			for k := range keys {
				return k
			}
		}
		return repo
	}

	// The runs in the report, grouped by change.
	changes := map[string][]*runInfo{}
	var changeOrder []string
	for _, id := range order {
		r := runs[id]
		r.repo = qualify(r.repo)
		if r.start.IsZero() || r.start.Before(opts.Since) || !matchRepo(opts.Repo, r.repo) {
			continue
		}
		c := r.changeIdentity()
		if _, ok := changes[c]; !ok {
			changeOrder = append(changeOrder, c)
		}
		changes[c] = append(changes[c], r)
	}

	byRepo := map[string]*Totals{}
	totals := func(repo string) *Totals {
		t := byRepo[repo]
		if t == nil {
			t = newTotals(repo)
			byRepo[repo] = t
		}
		return t
	}
	rep.Overall = *newTotals("")

	for _, c := range changeOrder {
		rs := changes[c]
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].start.Before(rs[j].start) })
		ch := Change{Key: changeKey(rs), Repo: lastRepo(rs), Runs: len(rs), FollowUps: []FollowUp{}}
		repoT := totals(ch.Repo)
		for _, t := range []*Totals{&rep.Overall, repoT} {
			t.Changes++
		}
		for i, r := range rs {
			var wait int64
			if i > 0 && !rs[i-1].end.IsZero() && r.start.After(rs[i-1].end) {
				wait = r.start.Sub(rs[i-1].end).Milliseconds()
			}
			followUp := i > 0 || followUpReasons[r.reason]
			ch.AgentMS += r.agent
			ch.WaitMS += wait
			if followUp {
				ch.FollowUps = append(ch.FollowUps, FollowUp{RunID: r.id, Reason: reasonOf(r), WaitMS: wait})
			}
			for _, t := range []*Totals{&rep.Overall, repoT} {
				t.addRun(r)
				t.WaitMS += wait
				if followUp {
					t.FollowUps++
					t.FollowUpReasons[reasonOf(r)]++
				}
			}
		}
		if wait, ok := approvalWait(rs, approvals); ok {
			ch.ApprovalWaitMS = wait
			for _, t := range []*Totals{&rep.Overall, repoT} {
				t.ApprovalWaitMS += wait
				t.Approvals++
			}
		}
		rep.Changes = append(rep.Changes, ch)
	}

	for _, j := range jobs {
		j.repo = qualify(j.repo)
		if j.start.Before(opts.Since) || !matchRepo(opts.Repo, j.repo) {
			continue
		}
		ts := []*Totals{&rep.Overall}
		if j.repo != "" {
			ts = append(ts, totals(j.repo))
		}
		for _, t := range ts {
			if j.review {
				t.Reviews++
				t.ReviewMS += j.ms
			} else {
				t.Consults++
				t.ConsultMS += j.ms
			}
		}
	}

	for _, t := range byRepo {
		rep.Repos = append(rep.Repos, *t)
	}
	sort.Slice(rep.Repos, func(i, j int) bool {
		a, b := rep.Repos[i], rep.Repos[j]
		if a.AgentMS != b.AgentMS {
			return a.AgentMS > b.AgentMS
		}
		return a.Repo < b.Repo
	})
	sort.SliceStable(rep.Changes, func(i, j int) bool { return rep.Changes[i].Runs > rep.Changes[j].Runs })
	if rep.Repos == nil {
		rep.Repos = []Totals{}
	}
	if rep.Changes == nil {
		rep.Changes = []Change{}
	}
	return rep
}

func newTotals(repo string) *Totals {
	return &Totals{Repo: repo, Reasons: map[string]int{}, Checks: map[string]*CheckTotals{}, FollowUpReasons: map[string]int{}}
}

func (t *Totals) addRun(r *runInfo) {
	t.Runs++
	t.Reasons[reasonOf(r)]++
	if r.end.IsZero() {
		t.Unfinished++
	}
	if !r.timed && r.agent > 0 {
		t.Untimed++
	}
	t.SetupMS += r.setup
	t.AgentMS += r.agent
	t.FinalizeMS += r.final
	for _, c := range r.checks {
		ct := t.Checks[c.kind]
		if ct == nil {
			ct = &CheckTotals{}
			t.Checks[c.kind] = ct
		}
		ct.Count++
		ct.MS += c.ms
		if c.success != nil && !*c.success {
			ct.Failed++
		}
	}
	if r.cost != nil {
		t.CostUSD += *r.cost
		t.CostRuns++
	}
}

func reasonOf(r *runInfo) string {
	if r.reason == "" {
		return unknownReason
	}
	return r.reason
}

// observe takes one of the run's events, at its timestamp.
func (r *runInfo) observe(typ string, at time.Time, d data) {
	if r.start.IsZero() || at.Before(r.start) {
		r.start = at
	}
	// The fields every run event carries; later events know more (a PR, a
	// valley's name for the repo).
	if v := d.str("repo_key"); v != "" {
		r.repo = v
	}
	if v := d.str("change_id"); v != "" {
		r.changeID = v
	}
	if v := d.str("pr_number"); v != "" {
		r.pr = v
	}
	if v := d.str("dispatch_reason"); v != "" && r.reason == "" {
		r.reason = v
	}

	switch typ {
	case event.AgentStarted:
		r.agentStartedAt = at
		if r.repo == "" {
			r.repo = d.str("target_repo") // before repo_key
		}
	case event.PhaseCompleted:
		ms := d.int("duration_ms")
		switch d.str("phase") {
		case event.PhaseSetup:
			r.setup = ms
			if t, err := time.Parse(time.RFC3339Nano, d.str("started_at")); err == nil {
				r.start = t
			}
		case event.PhaseAgent:
			r.agent, r.timed = ms, true
			if c, ok := d.float("cost_usd"); ok {
				r.cost = &c
			}
		case event.PhaseFinalize:
			r.final = ms
			r.end = at
			if t, err := time.Parse(time.RFC3339Nano, d.str("ended_at")); err == nil {
				r.end = t
			}
		}
	case event.CheckCompleted:
		c := check{kind: d.str("kind"), ms: d.int("duration_ms")}
		if v, ok := d["success"].(bool); ok {
			c.success = &v
		}
		r.checks = append(r.checks, c)
	case event.AgentCompleted, event.AgentPaused, event.AgentNeedsAttention:
		r.terminalAt = at
		if c, ok := d.float("cost_usd"); ok && r.cost == nil {
			r.cost = &c
		}
	case event.AgentPRCreated:
		r.terminalAt = at
		if r.repo == "" || !strings.Contains(r.repo, "/") {
			if repo := repoFromPRURL(d.str("pr_url")); repo != "" {
				r.repo = repo
			}
		}
	case event.AgentBranchPushed:
		r.terminalAt = at
		if r.repo == "" {
			r.repo = shortName(d.str("repo"))
		}
	case event.AgentChangeReady:
		r.terminalAt = at
		if v := d.str("repo"); v != "" && !strings.HasPrefix(r.repo, "valley:") {
			r.repo = "valley:" + v
		}
	}
}

// finish settles what the run's events leave open: a run from before phase
// events ends at its terminal event, and its agent time runs there from
// agent:started.
func (r *runInfo) finish() {
	if r.finished {
		return
	}
	r.finished = true
	if r.end.IsZero() && !r.terminalAt.IsZero() {
		r.end = r.terminalAt
	}
	if !r.timed && !r.agentStartedAt.IsZero() && r.terminalAt.After(r.agentStartedAt) {
		r.agent = r.terminalAt.Sub(r.agentStartedAt).Milliseconds()
	}
}

// changeIdentity is what groups runs into a change: the change ID, which
// every run since change IDs has and follow-ups inherit, else the PR, else
// the run itself.
func (r *runInfo) changeIdentity() string {
	switch {
	case r.changeID != "":
		return "change:" + r.changeID
	case r.pr != "":
		return "pr:" + r.repo + "#" + r.pr
	}
	return "run:" + r.id
}

// changeKey names a change's runs as runFields' key does: by its PR when a
// run has one, else by its change ID, else by its lone run.
func changeKey(rs []*runInfo) string {
	repo := lastRepo(rs)
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].pr != "" {
			return repo + "#" + rs[i].pr
		}
	}
	if rs[0].changeID != "" {
		return repo + "@" + rs[0].changeID
	}
	return rs[0].id
}

func lastRepo(rs []*runInfo) string {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].repo != "" {
			return rs[i].repo
		}
	}
	return ""
}

// approvalWait is the time from the run that ended last before the change's
// first approval to that approval.
func approvalWait(rs []*runInfo, approvals []approval) (int64, bool) {
	key := changeKey(rs)
	if !strings.Contains(key, "#") {
		return 0, false
	}
	var first time.Time
	for _, a := range approvals {
		if strings.EqualFold(a.key, key) && (first.IsZero() || a.at.Before(first)) {
			first = a.at
		}
	}
	if first.IsZero() {
		return 0, false
	}
	var last time.Time
	for _, r := range rs {
		if !r.end.IsZero() && !r.end.After(first) && r.end.After(last) {
			last = r.end
		}
	}
	if last.IsZero() {
		return 0, false
	}
	return first.Sub(last).Milliseconds(), true
}

// prKey is the repo-qualified key of a PR event: its key, else built from
// its PR URL.
func prKey(d data, runID string) string {
	if k := d.str("key"); strings.Contains(k, "#") {
		return k
	}
	repo, pr := repoFromPRURL(d.str("pr_url")), d.str("pr_number")
	if pr == "" && isDigits(runID) {
		pr = runID
	}
	if repo == "" || pr == "" {
		return ""
	}
	return repo + "#" + pr
}

// repoFromPRURL is the owner/repo of a GitHub PR URL, or "".
func repoFromPRURL(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	s, ok := strings.CutPrefix(s, "github.com/")
	if !ok {
		return ""
	}
	parts := strings.Split(s, "/")
	if len(parts) < 4 || parts[0] == "" || parts[1] == "" || parts[2] != "pull" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func shortName(repo string) string {
	name := strings.TrimSuffix(strings.TrimRight(repo, "/"), ".git")
	if strings.Contains(name, "://") || strings.Contains(name, "@") || strings.HasPrefix(name, "/") {
		if i := strings.LastIndexAny(name, "/:"); i >= 0 {
			name = name[i+1:]
		}
	}
	return name
}

// matchRepo reports whether repo is the one want names: its key, its name
// after valley:, or its last path component, in any case. "" wants all.
func matchRepo(want, repo string) bool {
	if want == "" {
		return true
	}
	want, repo = strings.ToLower(want), strings.ToLower(repo)
	return want == repo || want == strings.TrimPrefix(repo, "valley:") || want == repoName(repo)
}

// isQualified reports whether a repo key says where the repo lives:
// owner/repo or valley:<name>.
func isQualified(repo string) bool {
	return strings.Contains(repo, "/") || strings.HasPrefix(repo, "valley:")
}

// repoName is a repo key's short name: after valley:, its last path
// component.
func repoName(repo string) string {
	name := strings.TrimPrefix(repo, "valley:")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// isRunID reports whether id is a run's (YYYYMMDD-HHMM-xxxxxxxx), not a
// PR number or a consult's or review's.
func isRunID(id string) bool {
	if id == "" || isDigits(id) {
		return false
	}
	for _, p := range []string{"consult-", "review-", "panel-"} {
		if strings.HasPrefix(id, p) {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// data reads an event's payload as JSON decoded it.
type data map[string]interface{}

func (d data) str(k string) string {
	switch v := d[k].(type) {
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

func (d data) int(k string) int64 {
	if v, ok := d[k].(float64); ok {
		return int64(v)
	}
	return 0
}

func (d data) float(k string) (float64, bool) {
	v, ok := d[k].(float64)
	return v, ok
}

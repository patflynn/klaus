package cmd

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/jobs"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/run"
)

// rowCategory groups dashboard rows by whose move it is; the operator's come
// first.
type rowCategory int

const (
	catYours    rowCategory = iota // the operator's move: needs attention, a change to approve or look at, a draft, a branch to submit, a PR ready to approve
	catAgents                      // an agent is running on it
	catPipeline                    // an open PR the pipeline is moving (CI, conflicts, behind, approved and waiting), or a change the integrator has to decide
	catDone                        // merged, closed, integrated, gone, withdrawn, no access, or an agent that exited
)

// categoryNames label the categories in the summary line.
var categoryNames = [...]string{catYours: "yours", catAgents: "agents", catPipeline: "pipeline", catDone: "done"}

// defaultHideFinishedAfter is how long a done row stays on the dashboard
// before it is hidden behind "show all".
const defaultHideFinishedAfter = time.Hour

// hideFinishedAfter resolves the hide threshold from config. A zero value
// means "use the default"; a negative value means never hide (returns 0).
func hideFinishedAfter(cfg *config.DashboardConfig) time.Duration {
	if cfg == nil || cfg.HideFinishedAfterMinutes == 0 {
		return defaultHideFinishedAfter
	}
	if cfg.HideFinishedAfterMinutes < 0 {
		return 0
	}
	return time.Duration(cfg.HideFinishedAfterMinutes) * time.Minute
}

// dashRow is one line of the dashboard run list: a PR, with every agent
// that worked on it and every run of its change that has no PR; a change
// no PR carries, with every run on it; or a bare agent run with neither.
type dashRow struct {
	key      string // stable identity, so the selection survives reordering
	repo     string
	prNum    string       // "" for a change or a bare agent
	changeID string       // the change a change row groups; "" for a PR or a bare agent
	runs     []*run.State // a PR's agents, then its change's runs without a PR; runs on this change (newest first); or the single bare agent
	running  []*run.State // the subset of runs whose agent is running
	ps       *prStatus    // GitHub status, nil until fetched or for bare agents
	category rowCategory
	lastSeen time.Time // most recent activity across runs, for ordering and hiding
	hideable bool      // merged, closed, integrated, gone, no access or cleaned up: may be hidden once old
	// attention is set when the row is the operator's because something
	// needs attention (salvaged work, a stalled or budget-paused PR).
	attention bool
	// pushed is set when the row shows where pushed work stands: a bare
	// pushed run, or a change with a version, the valley's or a local push.
	pushed bool
	// current is the run that pushed a change row's state: the local run that
	// pushed the active version of a change the valley lists, or, for a
	// change it doesn't, the latest run that pushed. nil when none did, as
	// when the active version was marked ready elsewhere.
	current *run.State
	// verdict is the latest integration check of a bare pushed run; for a
	// change row, the valley's view of the change's active version, or else
	// the check of its current run.
	verdict integration.Verdict
	// change is how a valley knows a bare pushed run, or a change row's
	// active version: the latest check's view, or else what finalize
	// recorded. nil for a branch no valley knows.
	change *integration.Change
	// unplaced is the head of a change's latest local push when the valley
	// lists the change but doesn't hold that head as a version of it, as
	// when its ready failed. The row doesn't show it; the footer notes it.
	unplaced string
	// provisional marks a SUBMIT or READY row with no definite integration
	// verdict yet: it may turn out to have landed long ago.
	provisional bool
	// outdated marks a verdict and change kept from an earlier check because
	// the latest couldn't ask the valley (integration.Result.Outdated).
	outdated bool
}

// state returns the run that speaks for the row: a PR's first agent, which
// carries the PR URL and is the one marked by approve; a change's current
// run, or else its latest; or the bare agent. Where a change stands comes
// from the row (pushed, verdict, change), not from this run.
func (r dashRow) state() *run.State {
	if r.current != nil {
		return r.current
	}
	return r.runs[0]
}

// repoLabel names the row's repo: the valley's name for a valley change,
// else the short project name.
func (r dashRow) repoLabel() string {
	if r.change != nil && r.change.Repo != "" {
		return r.change.Repo
	}
	return shortRepoName(r.repo)
}

// idLabel names a row without a PR: a valley change's handle, else the
// first 8 characters of its change ID, or for a bare agent "agent:" and the
// tail of the run ID.
func (r dashRow) idLabel() string {
	switch {
	case r.change != nil:
		return r.change.Handle
	case r.changeID != "":
		return r.changeID[:min(8, len(r.changeID))]
	}
	return "agent:" + shortRunID(r.state().ID)
}

// pushedRun is the run whose push the row shows: a change's current run, or
// a bare pushed run. nil for a row with none, as for a change whose active
// version was marked ready elsewhere.
func (r dashRow) pushedRun() *run.State {
	switch {
	case r.changeID != "":
		return r.current
	case r.pushed:
		return r.runs[0]
	}
	return nil
}

// description is what the row is about: a valley change's title, else the
// subject of the head its current run pushed, else the prompt.
func (r dashRow) description() string {
	if r.change != nil && r.change.Title != "" {
		return r.change.Title
	}
	if r.current != nil && r.current.PushedSubject != "" {
		return r.current.PushedSubject
	}
	return r.state().Prompt
}

// buildRows turns the run states into dashboard rows, sorted by category
// (yours, agents, pipeline, done) and most recent first within each
// category. Sessions are excluded; agents on the same PR share a row, a PR
// named by the owner/repo of its URL and its number (prKey), so
// alice/widget#7 and bob/widget#7 never share one, whatever the project
// registry calls their repos. The
// runs without a PR on one change (changeKeys) share a row across repo
// groups: the row of the change's PR, the PR of the newest run on the
// change that has one, after the PR's own agents, unless where the change
// stands is the operator's to act on by itself (changeStandsAlone); else
// the change's own row.
func (m dashboardModel) buildRows() []dashRow {
	var rows []dashRow
	groups := groupByRepo(m.states)
	changeOf := changeKeys(m.states)
	prOfChange := make(map[integration.ChangeKey]int)     // the row of a change's PR
	prOwner := make(map[integration.ChangeKey]*run.State) // the newest run with a PR on each change
	prIndex := make(map[string]int)                       // the row of each PR, by its key
	for _, g := range groups {
		for _, s := range g.Runs {
			prNum := extractPRNumber(s)
			if prNum == "" {
				continue
			}
			key := prKey(s, g.Repo, prNum)
			i, ok := prIndex[key]
			if ok {
				rows[i].runs = append(rows[i].runs, s)
			} else {
				i = len(rows)
				prIndex[key] = i
				rows = append(rows, dashRow{
					key:   key,
					repo:  g.Repo,
					prNum: prNum,
					runs:  []*run.State{s},
					ps:    m.ghStatus[extractPRRef(s)],
				})
			}
			if k, ok := changeOf(s); ok && (prOwner[k] == nil || createdBefore(prOwner[k], s)) {
				prOfChange[k], prOwner[k] = i, s
			}
		}
	}
	// The runs without a PR on each change, and the repo group of each
	// change's first.
	var changeKeys []integration.ChangeKey
	changeRuns := make(map[integration.ChangeKey][]*run.State)
	changeRepo := make(map[integration.ChangeKey]string)
	for _, g := range groups {
		for _, s := range g.Runs {
			if extractPRNumber(s) != "" {
				continue
			}
			k, ok := changeOf(s)
			if !ok {
				rows = append(rows, dashRow{key: "run:" + s.ID, repo: g.Repo, runs: []*run.State{s}})
				continue
			}
			if _, seen := changeRuns[k]; !seen {
				changeKeys = append(changeKeys, k)
				changeRepo[k] = g.Repo
			}
			changeRuns[k] = append(changeRuns[k], s)
		}
	}
	for _, k := range changeKeys {
		if i, ok := prOfChange[k]; ok && !m.changeStandsAlone(changeRuns[k]) {
			rows[i].runs = append(rows[i].runs, changeRuns[k]...)
			continue
		}
		rows = append(rows, dashRow{
			key:      "change:" + k.Repo + "#" + k.ChangeID,
			repo:     changeRepo[k],
			changeID: k.ChangeID,
			runs:     changeRuns[k],
		})
	}
	superseded := supersededRuns(m.states, m.agentRunning)
	for i := range rows {
		if rows[i].changeID != "" {
			runs := rows[i].runs
			sort.SliceStable(runs, func(a, b int) bool { return createdBefore(runs[b], runs[a]) })
		}
		m.classify(&rows[i], superseded)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.category != b.category {
			return a.category < b.category
		}
		if !a.lastSeen.Equal(b.lastSeen) {
			return a.lastSeen.After(b.lastSeen)
		}
		return a.key < b.key
	})
	return rows
}

// prKey is the row key of the PR numbered prNum that run s has: "pr:", the
// owner/repo of its URL, lowercased as GitHub matches them, "#" and the
// number. A URL without an owner/repo falls back on the run's repo group.
func prKey(s *run.State, group, prNum string) string {
	repo := strings.ToLower(gh.OwnerRepoFromPRURL(extractPRRef(s)))
	if repo == "" {
		repo = group
	}
	return "pr:" + repo + "#" + prNum
}

// changeStandsAlone reports whether where a change stands, apart from its
// PR, is the operator's to act on by itself, so its runs without a PR keep
// the change's own row rather than folding into the PR's: a valley lists
// the change and its active version hasn't settled (a version to approve,
// or one the integrator turned down), or one of those runs pushed a branch
// that waits for integration, as far as its latest check knows. A push
// that landed or is gone folds in. runs are the change's runs without a PR.
func (m dashboardModel) changeStandsAlone(runs []*run.State) bool {
	if view, ok := m.changes[valleyKey(runs[0])]; ok && view.Change != nil && !view.Verdict.Settled() {
		return true
	}
	for _, s := range runs {
		if awaitsIntegration(s) && !m.integration[s.ID].Verdict.Settled() {
			return true
		}
	}
	return false
}

// classify fills in a row's running agents, category, recency and whether
// it may be hidden once old. A change row is judged by where the change
// stands (placeChange). A run's need for attention (needsAttention) stops
// counting once a later run took its work on (superseded): that run
// carries it, and shows it if it in turn ended needing attention.
// Precedence: a running agent makes it the agents'; then salvaged work no
// PR holds (any on a change, a run without a PR on a PR's row), or
// attention a run on a PR's row took on with other runs' work (TookOver),
// makes it yours; then merged, closed, no access, integrated, gone,
// withdrawn or superseded make it done; then needs attention, a pushed
// branch (to approve or submit, a draft, or whose change the integrator
// turned down or the valley is unsure of) or a PR ready to approve make it
// yours, except a change already approved, which is the pipeline's like any
// other open PR; an exited agent is done.
func (m dashboardModel) classify(r *dashRow, superseded map[string]bool) {
	if r.changeID != "" {
		m.placeChange(r)
	}
	merged, attention, owed := false, false, false
	for _, s := range r.runs {
		if m.agentRunning(s) {
			r.running = append(r.running, s)
		}
		if s.MergedAt != nil {
			merged = true
		}
		if needsAttention(s) && !superseded[s.ID] {
			attention = true
			owed = owed || r.prNum != "" && (s.PRURL == nil || len(s.TookOver) > 0)
		}
		if t := runLastActivity(s); t.After(r.lastSeen) {
			r.lastSeen = t
		}
	}
	owed = owed || r.changeID != "" && attention
	closed, noAccess := false, false
	if r.ps != nil {
		merged = merged || r.ps.State == "MERGED"
		closed = r.ps.State == "CLOSED"
		noAccess = r.ps.State == gh.StateNoAccess
	}
	if pps, ok := m.pipelineState(*r); ok {
		merged = merged || pps.Stage == pipeline.StageMerged
		attention = attention || pps.Stage == pipeline.StageStalled || pps.Stage == pipeline.StageBudgetPaused
	}
	if r.prNum == "" && r.changeID == "" && awaitsIntegration(r.runs[0]) {
		r.pushed = true
		r.verdict, r.change, r.outdated = m.runView(r.runs[0])
	}

	switch {
	case len(r.running) > 0:
		r.category = catAgents
	case owed:
		// Salvaged work that no later run took on, and that no PR holds,
		// is the operator's, whatever became of the change's PR or
		// version. So is the attention a run took on with other runs'
		// work and didn't hand on: its PR merged, closed or out of reach
		// settles nothing about it.
		r.category = catYours
		r.attention = true
	case merged || closed || noAccess || r.verdict.Settled():
		r.category = catDone
		r.hideable = true
	case attention:
		r.category = catYours
		r.attention = true
	case r.pushed && r.verdict == integration.Approved:
		r.category = catPipeline
	case r.pushed:
		// A pushed branch waits for the operator to approve or submit it, or
		// to look at what became of it, as far as anyone knows: Unknown (or
		// no check yet) is provisional.
		r.category = catYours
		r.provisional = r.verdict == integration.Unknown
	case r.prNum == "":
		r.category = catDone
		// A bare agent or change is hideable once its worktrees have been
		// cleaned up.
		r.hideable = true
		for _, s := range r.runs {
			r.hideable = r.hideable && s.Worktree == ""
		}
	case r.readyToApprove():
		r.category = catYours
	default:
		r.category = catPipeline
	}
}

// changeKeys returns changeOf for the runs among states: the change a run
// belongs to, named by its repo and its change ID. ok is false for a
// session or tracked PR, and for a run without a change ID or a known repo:
// such a run shares its change with none.
//
// The repo is the name the ValleyChecker places runs with
// (integration.RepoName: the valley's name for a valley's repo, else the
// short name of the target repo), qualified by the GitHub owner the run's
// repo is known by (repoOwner), so the same change ID in alice/widget and
// bob/widget names two changes. A run whose owner isn't known (a valley's
// repo, or a target named by its short name) takes the owner the other runs
// of its change are known by when there is exactly one, and none otherwise.
// A valley's listing names the change by the valley's repo name alone
// (valleyKey).
func changeKeys(states []*run.State) func(*run.State) (integration.ChangeKey, bool) {
	owners := make(map[integration.ChangeKey]map[string]bool) // the owners each change's runs are known by
	for _, s := range states {
		k, ok := nameChange(s)
		if o := repoOwner(s); ok && o != "" {
			if owners[k] == nil {
				owners[k] = make(map[string]bool)
			}
			owners[k][o] = true
		}
	}
	return func(s *run.State) (integration.ChangeKey, bool) {
		k, ok := nameChange(s)
		if !ok {
			return k, false
		}
		o := repoOwner(s)
		if o == "" && len(owners[k]) == 1 {
			for only := range owners[k] {
				o = only
			}
		}
		if o != "" {
			k.Repo = o + "/" + k.Repo
		}
		return k, true
	}
}

// nameChange is a run's change by its repo's name alone, as a valley's
// listing names it; ok as for changeKeys.
func nameChange(s *run.State) (integration.ChangeKey, bool) {
	if s.Type == "session" || s.Type == "track" || s.ChangeID == "" {
		return integration.ChangeKey{}, false
	}
	k := valleyKey(s)
	return k, k.Repo != ""
}

// repoOwner is the GitHub owner a run's repo is known by, lowercased: from
// its PR's URL, else from its RepoKey or its target repo when either is
// owner/repo. "" when none is, as for a valley's repo.
func repoOwner(s *run.State) string {
	slug := ""
	if s.PRURL != nil {
		slug = gh.OwnerRepoFromPRURL(*s.PRURL)
	}
	for _, ref := range []string{s.RepoKey, deref(s.TargetRepo)} {
		if slug == "" && ownerRepoRegex.MatchString(ref) {
			slug = ref
		}
	}
	owner, _, _ := strings.Cut(slug, "/")
	return strings.ToLower(owner)
}

// ownerRepoRegex matches a GitHub owner/repo, and not a path, a URL or a
// valley:<name> key.
var ownerRepoRegex = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// deref is *p, or "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// valleyKey is how a valley's listing names a run's change: by the repo
// name the ValleyChecker places runs with (integration.RepoName).
func valleyKey(s *run.State) integration.ChangeKey {
	return integration.ChangeKey{Repo: integration.RepoName(s), ChangeID: s.ChangeID}
}

// needsAttention reports whether a run ended needing the operator: finalize
// salvaged its work, or it failed after taking on the work of runs that
// needed attention (TookOver), whose attention is now its own.
func needsAttention(s *run.State) bool {
	return s.NeedsAttention != nil || s.FailureReason != nil && len(s.TookOver) > 0
}

// supersededRuns returns the IDs of the runs whose need for attention was
// handed off: a run of the same change records that it took their pushed
// work on (TookOver), and it is running or finished, so it carries that
// attention from then on. A successor that ended needing attention or
// failing is flagged itself (needsAttention); one that never finished and
// isn't running hands nothing off. Creation times play no part. Two runs
// that each record taking the other's work on hand nothing off to each
// other. The superseded runs' records stay as they are.
func supersededRuns(states []*run.State, running func(*run.State) bool) map[string]bool {
	changeOf := changeKeys(states)
	byID := make(map[string]*run.State, len(states))
	for _, s := range states {
		byID[s.ID] = s
	}
	out := make(map[string]bool)
	for _, b := range states {
		bk, ok := changeOf(b)
		if !ok || len(b.TookOver) == 0 || !(running(b) || b.CostUSD != nil || b.DurationMS != nil) {
			continue
		}
		for _, id := range b.TookOver {
			a := byID[id]
			if a == nil || a == b || !needsAttention(a) || slices.Contains(a.TookOver, b.ID) {
				continue
			}
			if ak, ok := changeOf(a); ok && ak == bk {
				out[id] = true
			}
		}
	}
	return out
}

// runView is where a pushed run stands by its latest check: the verdict, the
// valley's view of its version (else what finalize recorded), and whether
// that view is from an earlier check.
func (m dashboardModel) runView(s *run.State) (integration.Verdict, *integration.Change, bool) {
	res := m.integration[s.ID]
	if res.Change == nil {
		return res.Verdict, integration.RecordedChange(s), res.Outdated
	}
	return res.Verdict, res.Change, res.Outdated
}

// placeChange fills in where a change row stands. When the valley lists the
// change, its view of the change's active version decides: the verdict, the
// version, and the head to approve, wherever that version was marked ready.
// The current run is then the local run that pushed that head, if any; a
// later local push the valley doesn't hold as a version is only noted
// (unplaced). When the valley doesn't list the change (a repo with no
// valley, or an outage before the dashboard got a view), the latest run
// that pushed holds the state, as it would on its own. runs are newest
// first.
func (m dashboardModel) placeChange(r *dashRow) {
	var latest *run.State
	for _, s := range r.runs {
		if awaitsIntegration(s) {
			latest = s
			break
		}
	}
	view, ok := m.changes[valleyKey(r.runs[0])]
	if !ok || view.Change == nil {
		if r.current = latest; latest != nil {
			r.pushed = true
			r.verdict, r.change, r.outdated = m.runView(latest)
		}
		return
	}
	r.pushed = true
	r.verdict, r.change, r.outdated = view.Verdict, view.Change, view.Outdated
	for _, s := range r.runs {
		if awaitsIntegration(s) && *s.PushedSHA == view.Change.Head {
			r.current = s
			break
		}
	}
	if latest != nil && latest != r.current {
		if res, checked := m.integration[latest.ID]; checked && res.Change == nil && integration.RecordedChange(latest) == nil {
			r.unplaced = *latest.PushedSHA
		}
	}
	// A view kept through an outage gives way to git finding the active
	// version landed, as a run's own view does (keepValleyViews).
	if r.outdated && r.current != nil {
		if res := m.integration[r.current.ID]; res.Change == nil && res.Verdict.Landed() {
			r.verdict, r.outdated = res.Verdict, false
		}
	}
}

// pipelineState returns the pipeline's state for a PR row. The pipeline
// keys PRs by number alone, so a state counts only if it tracks this row's
// PR, not a same-numbered PR in another repo.
func (m dashboardModel) pipelineState(r dashRow) (*pipeline.PRPipelineState, bool) {
	pps, ok := m.pipelineStates[r.prNum]
	if !ok || r.prNum == "" {
		return nil, false
	}
	for _, s := range r.runs {
		if extractPRRef(s) == pps.PRURL {
			return pps, true
		}
	}
	return nil, false
}

// readyToApprove reports whether a PR row waits on the operator's approval:
// open, not a draft, ready to merge, and not yet approved with klaus approve.
func (r dashRow) readyToApprove() bool {
	ps := r.ps
	return ps != nil && ps.State == "OPEN" && !ps.Draft &&
		computeMergeStatus(ps.CI, ps.Conflicts, ps.ReviewDecision, ps.BehindBy) == "ready" &&
		!isAnyRunApproved(r.runs)
}

// awaitsIntegration reports whether a run finished by pushing its branch,
// with nothing else (a running agent aside) deciding how it is shown: no PR,
// no merge and nothing needing attention. Only such runs get an integration
// check.
func awaitsIntegration(s *run.State) bool {
	return s.Type != "session" && s.Type != "track" && integration.Applies(s) &&
		s.PRURL == nil && s.MergedAt == nil && s.NeedsAttention == nil
}

// runLastActivity returns the latest time known for a run: its creation,
// finish (creation plus duration), approval or merge.
func runLastActivity(s *run.State) time.Time {
	var latest time.Time
	consider := func(t time.Time) {
		if t.After(latest) {
			latest = t
		}
	}
	if t, err := time.Parse(time.RFC3339, s.CreatedAt); err == nil {
		consider(t)
		if s.DurationMS != nil && *s.DurationMS > 0 {
			consider(t.Add(time.Duration(*s.DurationMS) * time.Millisecond))
		}
	}
	for _, ts := range []*string{s.ApprovedAt, s.MergedAt} {
		if ts == nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339, *ts); err == nil {
			consider(t)
		}
	}
	return latest
}

// noteFinished records when a row the dashboard watched while it was still in
// progress is first seen done. A PR merged on GitHub has no local merge time,
// and a branch integrated on origin has no local time at all, so this keeps
// such a row visible for the full threshold after the dashboard saw it finish
// instead of hiding it at once. A SUBMIT row without a definite integration
// verdict doesn't count as watched: until the first check it may be work
// that landed long ago.
func (m *dashboardModel) noteFinished(rows []dashRow, now time.Time) {
	if m.seenUnfinished == nil {
		m.seenUnfinished = make(map[string]bool)
	}
	if m.finishedAt == nil {
		m.finishedAt = make(map[string]time.Time)
	}
	for _, r := range rows {
		if r.category != catDone {
			if !r.provisional {
				m.seenUnfinished[r.key] = true
			}
			delete(m.finishedAt, r.key)
			continue
		}
		if _, ok := m.finishedAt[r.key]; !ok && m.seenUnfinished[r.key] {
			m.finishedAt[r.key] = now
		}
	}
}

// isHidden reports whether a row is an old done row that the dashboard hides
// until "show all" is toggled. Rows in yours, agents and pipeline are never
// hidden.
func (m dashboardModel) isHidden(r dashRow, now time.Time) bool {
	if m.showAll || m.hideAfter <= 0 || r.category != catDone || !r.hideable {
		return false
	}
	finished := r.lastSeen
	if t, ok := m.finishedAt[r.key]; ok && t.After(finished) {
		finished = t
	}
	return now.Sub(finished) > m.hideAfter
}

// dashView is the resolved state of the run list for one render: the rows on
// display, how many fit, and which one is selected and scrolled to.
type dashView struct {
	all    []dashRow // every row, including hidden ones
	rows   []dashRow // rows on display, in order
	hidden int       // rows hidden as old done rows
	chrome []chromeItem
	bodyH  int // rows that fit in the viewport
	cursor int // index into rows of the selected row
	offset int // index into rows of the first row in the viewport
	// jobs are the jobs section's jobs; the first jobsShown fit the pane.
	// jobFocus says one of those is selected, jobs[jobCursor], rather than
	// a row.
	jobs      []*jobs.Job
	jobsShown int
	jobCursor int
	jobFocus  bool
}

// resolve builds the rows, fits the layout to the pane and resolves the
// selection and scroll position.
func (m dashboardModel) resolve(now time.Time) dashView {
	return m.resolveRows(m.buildRows(), now)
}

func (m dashboardModel) resolveRows(all []dashRow, now time.Time) dashView {
	v := dashView{all: all}
	for _, r := range all {
		if m.isHidden(r, now) {
			v.hidden++
			continue
		}
		v.rows = append(v.rows, r)
	}
	v.jobs = m.recentJobList(now)
	v.chrome, v.bodyH = m.fitLayout(len(v.rows), len(v.jobs))
	for _, c := range v.chrome {
		if c.kind == chromeJob {
			v.jobsShown++
		}
	}
	v.jobCursor = clampCursor(m.jobCursor, v.jobsShown)
	for i := 0; i < v.jobsShown; i++ {
		if v.jobs[i].ID == m.jobSel {
			v.jobCursor = i
			break
		}
	}
	v.jobFocus = m.jobFocus && v.jobsShown > 0

	v.cursor = clampCursor(m.cursor, len(v.rows))
	for i, r := range v.rows {
		if r.key == m.selKey {
			v.cursor = i
			break
		}
	}
	v.offset = scrollOffset(m.offset, v.cursor, len(v.rows), v.bodyH)
	return v
}

// syncView re-resolves the list after anything that can change it, so the
// selection stays on the same row and in view when rows reorder, appear or
// disappear. If the selected row is gone, the selection stays at the same
// position, clamped to the list.
func (m *dashboardModel) syncView(now time.Time) {
	all := m.buildRows()
	m.noteFinished(all, now)
	v := m.resolveRows(all, now)
	m.setSelection(v, v.cursor, v.offset)
	// The selected job likewise, while one is.
	m.jobFocus, m.jobCursor, m.jobSel = v.jobFocus, v.jobCursor, ""
	if v.jobsShown > 0 {
		m.jobSel = v.jobs[v.jobCursor].ID
	}
}

// setSelection selects rows[cursor] and scrolls from offset just enough to
// keep it visible.
func (m *dashboardModel) setSelection(v dashView, cursor, offset int) {
	if len(v.rows) == 0 {
		m.cursor, m.offset, m.selKey = 0, 0, ""
		return
	}
	m.cursor = clampCursor(cursor, len(v.rows))
	m.selKey = v.rows[m.cursor].key
	m.offset = scrollOffset(offset, m.cursor, len(v.rows), v.bodyH)
}

// moveSelection moves the selection by delta rows plus pages viewport pages,
// turning the viewport by the same number of pages.
func (m *dashboardModel) moveSelection(delta, pages int) {
	v := m.resolve(time.Now())
	page := pages * max(v.bodyH, 1)
	m.setSelection(v, v.cursor+delta+page, v.offset+page)
}

// selectEdge selects the first or last row.
func (m *dashboardModel) selectEdge(last bool) {
	v := m.resolve(time.Now())
	if last {
		m.setSelection(v, len(v.rows)-1, len(v.rows))
		return
	}
	m.setSelection(v, 0, 0)
}

// scrollOffset returns the first row to show so that the cursor row is
// visible, starting from offset and moving as little as possible. A
// non-positive height means the pane height is unknown and everything shows.
func scrollOffset(offset, cursor, n, height int) int {
	if height <= 0 || n <= height {
		return 0
	}
	if cursor < offset {
		offset = cursor
	}
	if cursor >= offset+height {
		offset = cursor - height + 1
	}
	return clamp(offset, 0, n-height)
}

// clampCursor keeps a cursor index within [0, n-1]; it returns 0 when the list
// is empty.
func clampCursor(cursor, n int) int {
	if n <= 0 {
		return 0
	}
	if cursor < 0 {
		return 0
	}
	if cursor >= n {
		return n - 1
	}
	return cursor
}

// selectedRow returns the currently selected row.
func (m dashboardModel) selectedRow() (dashRow, bool) {
	v := m.resolve(time.Now())
	if len(v.rows) == 0 {
		return dashRow{}, false
	}
	return v.rows[v.cursor], true
}

// selectedPR returns the selected row if it is a PR, noting a hint when the
// selection is a bare agent that has no PR to act on.
func (m *dashboardModel) selectedPR(action string) (dashRow, bool) {
	r, ok := m.selectedRow()
	if !ok {
		return dashRow{}, false
	}
	if r.prNum == "" {
		m.noteError(fmt.Sprintf("%s: %s has no PR", action, r.idLabel()))
		return dashRow{}, false
	}
	return r, true
}

// coordinatorPane returns the tmux pane id of the coordinator (Claude) session
// for the current dashboard, or "" if it can't be determined (e.g. an older
// session that predates the persisted coordinator pane).
func (m dashboardModel) coordinatorPane() string {
	sessionID := os.Getenv(sessionIDEnv)
	if sessionID == "" {
		return ""
	}
	st, err := m.store.Load(sessionID)
	if err != nil || st == nil || st.CoordinatorPane == nil {
		return ""
	}
	return *st.CoordinatorPane
}

// discussPR sends a literal "WRT PR#<num>: " prompt prefix to the coordinator
// pane and switches tmux focus there. It deliberately does not send a newline —
// the user finishes and submits the prompt manually.
func (m dashboardModel) discussPR(prNum, pane string) error {
	ctx := context.Background()
	if err := m.tmux.SendKeys(ctx, pane, fmt.Sprintf("WRT PR#%s: ", prNum)); err != nil {
		return err
	}
	return m.tmux.SelectPane(ctx, pane)
}

// noteError appends a transient error line to the dashboard, keeping only the
// most recent few.
func (m *dashboardModel) noteError(msg string) {
	m.recentErrors = append(m.recentErrors, dashboardError{Time: time.Now(), Message: msg})
	if len(m.recentErrors) > 3 {
		m.recentErrors = m.recentErrors[len(m.recentErrors)-3:]
	}
}

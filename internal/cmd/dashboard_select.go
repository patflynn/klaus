package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/patflynn/klaus/internal/config"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
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
	catDone                        // merged, closed, integrated, gone, withdrawn, superseded, no access, or an agent that exited
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
// that worked on it; a change, with every run on it that has no PR; or a
// bare agent run with neither.
type dashRow struct {
	key      string // stable identity, so the selection survives reordering
	repo     string
	prNum    string       // "" for a change or a bare agent
	changeID string       // the change a change row groups; "" for a PR or a bare agent
	runs     []*run.State // agents on this PR, runs on this change (newest first), or the single bare agent
	running  []*run.State // the subset of runs whose agent is running
	ps       *prStatus    // GitHub status, nil until fetched or for bare agents
	category rowCategory
	lastSeen time.Time // most recent activity across runs, for ordering and hiding
	hideable bool      // merged, closed, integrated, gone, no access or cleaned up: may be hidden once old
	// attention is set when the row is the operator's because something
	// needs attention (salvaged work, a stalled or budget-paused PR).
	attention bool
	// current is the run that holds a change row's state (currentRun): the
	// latest that pushed the change's active version. nil when none pushed.
	current *run.State
	// verdict is the latest integration check of a bare pushed run, or of a
	// change row's current run.
	verdict integration.Verdict
	// change is how a valley knows a bare pushed run, or a change row's
	// current run: the latest check's view, or else what finalize recorded.
	// nil for a branch no valley knows.
	change *integration.Change
	// provisional marks a SUBMIT or APPROVE row with no definite integration
	// verdict yet: it may turn out to have landed long ago.
	provisional bool
	// outdated marks a verdict and change kept from an earlier check because
	// the latest couldn't ask the valley (integration.Result.Outdated).
	outdated bool
}

// state returns the run that speaks for the row: a PR's first agent, which
// carries the PR URL and is the one marked by approve; a change's current
// run, or else its latest; or the bare agent.
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
// category. Sessions are excluded; agents on the same PR share a row, and
// so do runs without a PR on the same change (changeOf), across repo groups.
func (m dashboardModel) buildRows() []dashRow {
	var rows []dashRow
	changeIndex := make(map[changeKey]int)
	for _, g := range groupByRepo(m.states) {
		prIndex := make(map[string]int)
		for _, s := range g.Runs {
			prNum := extractPRNumber(s)
			if prNum == "" {
				k, ok := changeOf(s)
				if !ok {
					rows = append(rows, dashRow{key: "run:" + s.ID, repo: g.Repo, runs: []*run.State{s}})
					continue
				}
				if i, ok := changeIndex[k]; ok {
					rows[i].runs = append(rows[i].runs, s)
					continue
				}
				changeIndex[k] = len(rows)
				rows = append(rows, dashRow{
					key:      "change:" + k.repo + "#" + k.changeID,
					repo:     g.Repo,
					changeID: k.changeID,
					runs:     []*run.State{s},
				})
				continue
			}
			if i, ok := prIndex[prNum]; ok {
				rows[i].runs = append(rows[i].runs, s)
				continue
			}
			prIndex[prNum] = len(rows)
			rows = append(rows, dashRow{
				key:   "pr:" + g.Repo + "#" + prNum,
				repo:  g.Repo,
				prNum: prNum,
				runs:  []*run.State{s},
				ps:    m.ghStatus[extractPRRef(s)],
			})
		}
	}
	for i := range rows {
		if rows[i].changeID != "" {
			runs := rows[i].runs
			sort.SliceStable(runs, func(a, b int) bool { return createdBefore(runs[b], runs[a]) })
		}
		m.classify(&rows[i])
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

// classify fills in a row's running agents, category, recency and whether
// it may be hidden once old. A change row is judged by its current run
// (currentRun), and salvaged work counts only from that run on: a later run
// that pushed continued it. Precedence: a running agent makes it the
// agents'; then, on a change, salvaged work makes it yours; then merged,
// closed, no access, integrated, gone, withdrawn or superseded make it
// done; then needs attention, a pushed branch (to approve
// or submit, a draft, or whose change the integrator turned down or the
// valley is unsure of) or a PR ready to approve make it yours, except a
// change already approved, which is the pipeline's like any other open PR;
// an exited agent is done.
func (m dashboardModel) classify(r *dashRow) {
	if r.changeID != "" {
		r.current = m.currentRun(r.runs)
	}
	merged, attention := false, false
	for _, s := range r.runs {
		if m.agentRunning(s) {
			r.running = append(r.running, s)
		}
		if s.MergedAt != nil {
			merged = true
		}
		if s.NeedsAttention != nil && (r.current == nil || !createdBefore(s, r.current)) {
			attention = true
		}
		if t := runLastActivity(s); t.After(r.lastSeen) {
			r.lastSeen = t
		}
	}
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
	pushed := r.prNum == "" && awaitsIntegration(r.state())
	if pushed {
		res := m.integration[r.state().ID]
		r.verdict, r.change, r.outdated = res.Verdict, res.Change, res.Outdated
		if r.change == nil {
			r.change = integration.RecordedChange(r.state())
		}
	}

	switch {
	case len(r.running) > 0:
		r.category = catAgents
	case r.changeID != "" && attention:
		// Work salvaged after the change's current version is the
		// operator's, whatever became of that version.
		r.category = catYours
		r.attention = true
	case merged || closed || noAccess || r.verdict.Settled():
		r.category = catDone
		r.hideable = true
	case attention:
		r.category = catYours
		r.attention = true
	case pushed && r.verdict == integration.Approved:
		r.category = catPipeline
	case pushed:
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

// changeKey identifies a change across runs: its repo, by the rule the
// ValleyChecker places runs with (integration.RepoName), and its change ID.
type changeKey struct{ repo, changeID string }

// changeOf returns the change a run belongs to. ok is false for a session or
// tracked PR, and for a run without a change ID or a known repo: such a run
// shares its change with none.
func changeOf(s *run.State) (changeKey, bool) {
	if s.Type == "session" || s.Type == "track" || s.ChangeID == "" {
		return changeKey{}, false
	}
	repo := integration.RepoName(s)
	return changeKey{repo, s.ChangeID}, repo != ""
}

// currentRun returns the run that holds a change's state: of the runs that
// pushed and await integration, the latest whose version the valley doesn't
// place behind another (superseded, or landed before another version became
// active), so earlier versions fold into the change; else the latest of
// them, when every one of their versions gave way to one from elsewhere. A
// run the valley hasn't placed counts as current. nil when none pushed. runs
// are newest first.
func (m dashboardModel) currentRun(runs []*run.State) *run.State {
	var latest *run.State
	for _, s := range runs {
		if !awaitsIntegration(s) {
			continue
		}
		if res := m.integration[s.ID]; res.Verdict != integration.Superseded && (res.Change == nil || res.Change.Active) {
			return s
		}
		if latest == nil {
			latest = s
		}
	}
	return latest
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
	v.chrome, v.bodyH = m.fitLayout(len(v.rows))

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

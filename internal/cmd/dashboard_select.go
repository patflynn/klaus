package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/run"
)

// rowCategory orders dashboard rows: the operator's focus comes first.
type rowCategory int

const (
	catActive    rowCategory = iota // an agent is running on it
	catAttention                    // needs the operator: salvaged work, stalled or budget-paused PR
	catOpen                         // open PR with no agent running
	catFinished                     // merged or closed PR, or an agent that exited
)

// defaultHideFinishedAfter is how long a merged, closed or cleaned-up run
// stays on the dashboard before it is hidden behind "show all".
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

// dashRow is one line of the dashboard run list: either a PR (with every
// agent that worked on it) or a bare agent run that has no PR.
type dashRow struct {
	key      string // stable identity, so the selection survives reordering
	repo     string
	prNum    string       // "" for a bare agent
	runs     []*run.State // agents on this PR, or the single bare agent
	running  []*run.State // the subset of runs whose agent is running
	ps       *prStatus    // GitHub status, nil until fetched or for bare agents
	category rowCategory
	lastSeen time.Time // most recent activity across runs, for ordering and hiding
	hideable bool      // merged, closed or cleaned up: may be hidden once old
}

// state returns the row's first agent's run state, which carries the PR URL
// and is the one marked by approve.
func (r dashRow) state() *run.State {
	return r.runs[0]
}

// buildRows turns the run states into dashboard rows, sorted by category
// (active, needs attention, open PRs, finished) and most recent first within
// each category. Sessions are excluded; agents on the same PR share a row.
func (m dashboardModel) buildRows() []dashRow {
	var rows []dashRow
	for _, g := range groupByRepo(m.states) {
		prIndex := make(map[string]int)
		for _, s := range g.Runs {
			prNum := extractPRNumber(s)
			if prNum == "" {
				rows = append(rows, dashRow{key: "run:" + s.ID, repo: g.Repo, runs: []*run.State{s}})
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
				ps:    m.ghStatus[prNum],
			})
		}
	}
	for i := range rows {
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
// it may be hidden once old.
func (m dashboardModel) classify(r *dashRow) {
	merged, attention := false, false
	for _, s := range r.runs {
		if m.agentRunning(s) {
			r.running = append(r.running, s)
		}
		if s.MergedAt != nil {
			merged = true
		}
		if s.NeedsAttention != nil {
			attention = true
		}
		if t := runLastActivity(s); t.After(r.lastSeen) {
			r.lastSeen = t
		}
	}
	closed := false
	if r.ps != nil {
		merged = merged || r.ps.State == "MERGED"
		closed = r.ps.State == "CLOSED"
	}
	if pps, ok := m.pipelineStates[r.prNum]; ok && r.prNum != "" {
		merged = merged || pps.Stage == pipeline.StageMerged
		attention = attention || pps.Stage == pipeline.StageStalled || pps.Stage == pipeline.StageBudgetPaused
	}

	switch {
	case len(r.running) > 0:
		r.category = catActive
	case merged || closed:
		r.category = catFinished
		r.hideable = true
	case attention:
		r.category = catAttention
	case r.prNum != "":
		r.category = catOpen
	default:
		r.category = catFinished
		// A bare agent is hideable once its worktree has been cleaned up.
		r.hideable = r.state().Worktree == ""
	}
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
// progress is first seen finished. A PR merged on GitHub has no local merge
// time, so this keeps it visible for the full threshold after the dashboard
// saw it finish instead of hiding it at once.
func (m *dashboardModel) noteFinished(rows []dashRow, now time.Time) {
	if m.seenUnfinished == nil {
		m.seenUnfinished = make(map[string]bool)
	}
	if m.finishedAt == nil {
		m.finishedAt = make(map[string]time.Time)
	}
	for _, r := range rows {
		if r.category != catFinished {
			m.seenUnfinished[r.key] = true
			delete(m.finishedAt, r.key)
			continue
		}
		if _, ok := m.finishedAt[r.key]; !ok && m.seenUnfinished[r.key] {
			m.finishedAt[r.key] = now
		}
	}
}

// isHidden reports whether a row is an old finished run that the dashboard
// hides until "show all" is toggled. Active, attention and open rows are never
// hidden.
func (m dashboardModel) isHidden(r dashRow, now time.Time) bool {
	if m.showAll || m.hideAfter <= 0 || r.category != catFinished || !r.hideable {
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
	hidden int       // rows hidden as old finished runs
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
		m.noteError(fmt.Sprintf("%s: agent:%s has no PR", action, shortRunID(r.state().ID)))
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

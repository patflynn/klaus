package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// prEntry identifies a single selectable PR row in the dashboard. The ordering
// of a slice of prEntry mirrors the on-screen render order (repos sorted by
// name, then PRs in first-seen order within each repo), so a cursor index into
// that slice maps directly to a visible row.
type prEntry struct {
	repo  string
	prNum string
	state *run.State // first agent's run state for this PR (used for approval)
}

// selectablePRs builds the flat, ordered list of selectable PR rows. It must
// stay consistent with renderGroup: groupByRepo yields repos sorted by name,
// and within each repo PRs appear in first-seen order among non-session runs.
func selectablePRs(states []*run.State) []prEntry {
	var entries []prEntry
	for _, g := range groupByRepo(states) {
		seen := make(map[string]bool)
		for _, s := range g.Runs {
			if s.Type == "session" {
				continue
			}
			prNum := extractPRNumber(s)
			if prNum == "" || seen[prNum] {
				continue
			}
			seen[prNum] = true
			entries = append(entries, prEntry{repo: g.Repo, prNum: prNum, state: s})
		}
	}
	return entries
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

// reconcileSelection updates m.cursor after the state list changes. If the
// previously-selected PR still exists it keeps the selection on that PR;
// otherwise it clamps the cursor into the new list's bounds. prev is the
// selectable list computed from the states that were displayed before the
// reload.
func (m *dashboardModel) reconcileSelection(prev []prEntry) {
	entries := selectablePRs(m.states)
	if len(entries) == 0 {
		m.cursor = 0
		return
	}
	if m.cursor >= 0 && m.cursor < len(prev) {
		want := prev[m.cursor]
		for i, e := range entries {
			if e.repo == want.repo && e.prNum == want.prNum {
				m.cursor = i
				return
			}
		}
	}
	m.cursor = clampCursor(m.cursor, len(entries))
}

// selectedPR returns the repo and PR number of the currently selected row.
func (m dashboardModel) selectedPR() (repo, prNum string, ok bool) {
	entries := selectablePRs(m.states)
	if len(entries) == 0 {
		return "", "", false
	}
	e := entries[clampCursor(m.cursor, len(entries))]
	return e.repo, e.prNum, true
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
	if err != nil || st.CoordinatorPane == nil {
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

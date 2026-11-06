package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
)

// dashClear is the clear the x key asks the operator to confirm: the runs
// klaus cleanup --done would remove, judged by the dashboard's rows, and the
// finished jobs it drops from the jobs section.
type dashClear struct {
	remove []string // run IDs
	jobs   int
}

// clearedMsg reports a clear's removals once they are done.
type clearedMsg struct{ failed []string }

// planClear picks what the x key clears, as klaus cleanup --done would:
// every run of a finished PR or change and the older runs of an open one,
// but never a running run, one that needs attention, or a change's or PR's
// latest open run.
func (m dashboardModel) planClear(now time.Time) *dashClear {
	v := m.resolve(now)
	running := make(map[*run.State]bool)
	var groups []doneGroup
	var loose []*run.State
	for _, r := range v.all {
		for _, s := range r.running {
			running[s] = true
		}
		if r.prNum == "" && r.changeID == "" && r.finished == "" {
			loose = append(loose, r.runs...)
			continue
		}
		groups = append(groups, doneGroup{label: r.key, finished: r.finished, runs: r.runs})
	}
	handedOff := supersededRuns(m.states, func(s *run.State) bool { return running[s] })
	c := &dashClear{}
	for _, d := range planDone(groups, loose,
		func(s *run.State) bool { return running[s] },
		func(s *run.State) bool { return needsAttention(s) && !handedOff[s.ID] || s.SharedGitChanged() }) {
		if d.remove {
			c.remove = append(c.remove, d.state.ID)
		}
	}
	for _, j := range m.recentJobList(now) {
		if j.Finished() {
			c.jobs++
		}
	}
	return c
}

// prompt is the one line that asks to confirm the clear.
func (c *dashClear) prompt() string {
	return fmt.Sprintf("  clear finished: remove %d runs and hide %d finished jobs? y/n", len(c.remove), c.jobs)
}

// clearCmd removes the clear's runs as klaus cleanup does, leaving each
// one's spend in the session's ledger.
func (m dashboardModel) clearCmd(c *dashClear) tea.Cmd {
	store, tc := m.store, m.tmux
	return func() tea.Msg {
		ctx := context.Background()
		root, _ := git.RepoRoot()
		deps := DefaultCleanupDeps(tc)
		var failed []string
		for _, id := range c.remove {
			if err := cleanupOneTo(io.Discard, ctx, root, store, git.NewExecClient(), id, false, deps, tc); err != nil {
				failed = append(failed, id)
			}
		}
		return clearedMsg{failed: failed}
	}
}

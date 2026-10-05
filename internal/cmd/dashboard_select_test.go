package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
)

// captureTmux is a tmux.Client that records SendKeys/SelectPane calls so tests
// can assert on the literal text and pane id sent to the coordinator.
type captureTmux struct {
	tmux.Client
	sentPane  string
	sentKeys  string
	focusPane string
}

func (c *captureTmux) SendKeys(_ context.Context, paneID, keys string) error {
	c.sentPane = paneID
	c.sentKeys = keys
	return nil
}

func (c *captureTmux) SelectPane(_ context.Context, paneID string) error {
	c.focusPane = paneID
	return nil
}

func TestBuildRowsOneRowPerPRAndBareAgent(t *testing.T) {
	// Sessions get no row; a bare (no-PR) agent gets its own; agents on the
	// same PR share one row, which carries the first agent's state. Rows of the
	// same category are most recent first, across repos.
	states := []*run.State{
		{ID: "s1", Type: "session", Prompt: "session"},
		{ID: "first", Prompt: "p1", TargetRepo: strPtr("zzz"), PRURL: strPtr("https://github.com/o/zzz/pull/7"), CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "a2", Prompt: "p2", TargetRepo: strPtr("aaa"), PRURL: strPtr("https://github.com/o/aaa/pull/9"), CreatedAt: "2026-01-01T00:01:00Z"},
		{ID: "second", Prompt: "p3", TargetRepo: strPtr("zzz"), PRURL: strPtr("https://github.com/o/zzz/pull/7"), CreatedAt: "2026-01-01T00:02:00Z"},
		{ID: "bare", Prompt: "bare", TargetRepo: strPtr("aaa"), Worktree: "/wt", CreatedAt: "2026-01-01T00:03:00Z"},
	}
	m := dashboardModel{states: states, tmuxDeps: testDashboardTmuxDeps()}

	var got []string
	for _, r := range m.buildRows() {
		got = append(got, r.key)
	}
	// Both PRs are open; #7's latest activity (00:02) is newer than #9's. The
	// exited bare agent is finished, so it sorts last.
	want := []string{"pr:zzz#7", "pr:aaa#9", "run:bare"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("row keys = %v, want %v", got, want)
	}
	rows := m.buildRows()
	if len(rows[0].runs) != 2 || rows[0].state().ID != "first" {
		t.Errorf("PR #7 row runs = %d, state = %s; want 2 runs, first agent's state", len(rows[0].runs), rows[0].state().ID)
	}
}

func TestClampCursor(t *testing.T) {
	cases := []struct{ cursor, n, want int }{
		{0, 0, 0},
		{5, 0, 0},
		{-1, 3, 0},
		{1, 3, 1},
		{3, 3, 2},
		{99, 3, 2},
	}
	for _, c := range cases {
		if got := clampCursor(c.cursor, c.n); got != c.want {
			t.Errorf("clampCursor(%d, %d) = %d, want %d", c.cursor, c.n, got, c.want)
		}
	}
}

func TestSelectionFollowsRowWhenListChanges(t *testing.T) {
	prevStates := []*run.State{
		{ID: "a", Prompt: "p", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/1"), CreatedAt: "2026-01-01T00:02:00Z"},
		{ID: "b", Prompt: "p", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/2"), CreatedAt: "2026-01-01T00:01:00Z"},
		{ID: "c", Prompt: "p", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/3"), CreatedAt: "2026-01-01T00:00:00Z"},
	}
	m := dashboardModel{states: prevStates, tmuxDeps: testDashboardTmuxDeps(), cursor: 2}
	m.syncView(time.Now())
	if r, _ := m.selectedRow(); r.prNum != "3" {
		t.Fatalf("selected PR #%s, want #3", r.prNum)
	}

	// PR #1 is removed; #3 still exists but shifts to index 1.
	next, _ := m.Update(statesLoadedMsg{states: []*run.State{prevStates[1], prevStates[2]}})
	m = next.(dashboardModel)
	if r, _ := m.selectedRow(); r.prNum != "3" || m.cursor != 1 {
		t.Errorf("after reload selection = #%s at %d, want PR #3 at 1", r.prNum, m.cursor)
	}

	// PR #3 is removed; the selection stays at its position, clamped.
	next, _ = m.Update(statesLoadedMsg{states: []*run.State{prevStates[0]}})
	m = next.(dashboardModel)
	if r, _ := m.selectedRow(); r.prNum != "1" || m.cursor != 0 {
		t.Errorf("after removal selection = #%s at %d, want PR #1 at 0", r.prNum, m.cursor)
	}
}

func TestDiscussPRSendsLiteralPromptAndFocuses(t *testing.T) {
	fake := &captureTmux{}
	m := dashboardModel{tmux: fake}

	if err := m.discussPR("583", "%7"); err != nil {
		t.Fatalf("discussPR: %v", err)
	}
	if want := "WRT PR#583: "; fake.sentKeys != want {
		t.Errorf("SendKeys keys = %q, want %q", fake.sentKeys, want)
	}
	if fake.sentPane != "%7" {
		t.Errorf("SendKeys pane = %q, want %q", fake.sentPane, "%7")
	}
	if fake.focusPane != "%7" {
		t.Errorf("SelectPane pane = %q, want %q", fake.focusPane, "%7")
	}
}

func TestCoordinatorPaneFromSessionState(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	sessionID := "20260101-0000-sess"
	pane := "%3"
	if err := store.Save(&run.State{ID: sessionID, Type: "session", CoordinatorPane: &pane, CreatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv(sessionIDEnv, sessionID)

	m := dashboardModel{store: store}
	if got := m.coordinatorPane(); got != pane {
		t.Errorf("coordinatorPane() = %q, want %q", got, pane)
	}

	// A session without a coordinator pane (older session) yields "".
	if err := store.Save(&run.State{ID: sessionID, Type: "session", CreatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := m.coordinatorPane(); got != "" {
		t.Errorf("coordinatorPane() = %q, want empty for older session", got)
	}
}

func TestApproveSelectedPRMarksRightRun(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	states := []*run.State{
		{ID: "r1", Prompt: "p", Branch: "b1", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/11"), CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "r2", Prompt: "p", Branch: "b2", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/22"), CreatedAt: "2026-01-01T00:01:00Z"},
	}
	for _, s := range states {
		if err := store.Save(s); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	// #22 is the more recent open PR, so it is listed first: move down to
	// #11 and back up to #22, then approve with the 'a' key.
	var model tea.Model = dashboardModel{store: store, states: states, tmuxDeps: testDashboardTmuxDeps()}
	for _, k := range []string{"j", "k", "a"} {
		model, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	}

	s22, _ := store.Load("r2")
	if s22.Approved == nil || !*s22.Approved {
		t.Error("PR #22 should be approved")
	}
	s11, _ := store.Load("r1")
	if s11.Approved != nil {
		t.Error("PR #11 should not be approved")
	}
}

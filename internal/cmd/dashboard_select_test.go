package cmd

import (
	"context"
	"testing"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
)

// captureTmux is a tmux.Client that records SendKeys/SelectPane calls so tests
// can assert on the literal text and pane id sent to the coordinator.
type captureTmux struct {
	tmux.ExecClient
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

func TestSelectablePRsOrdering(t *testing.T) {
	// Two repos; aaa sorts before zzz. Within a repo, PRs appear in first-seen
	// order. A session run and a bare (no-PR) agent are not selectable.
	states := []*run.State{
		{ID: "s1", Type: "session", Prompt: "session"},
		{ID: "a1", Prompt: "p1", TargetRepo: strPtr("zzz"), PRURL: strPtr("https://github.com/o/zzz/pull/5"), CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "a2", Prompt: "p2", TargetRepo: strPtr("aaa"), PRURL: strPtr("https://github.com/o/aaa/pull/9"), CreatedAt: "2026-01-01T00:01:00Z"},
		{ID: "a3", Prompt: "p3", TargetRepo: strPtr("aaa"), PRURL: strPtr("https://github.com/o/aaa/pull/3"), CreatedAt: "2026-01-01T00:02:00Z"},
		{ID: "a4", Prompt: "bare", TargetRepo: strPtr("aaa"), CreatedAt: "2026-01-01T00:03:00Z"},
	}

	got := selectablePRs(states)
	want := []struct{ repo, pr string }{
		{"aaa", "9"},
		{"aaa", "3"},
		{"zzz", "5"},
	}
	if len(got) != len(want) {
		t.Fatalf("selectablePRs len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].repo != w.repo || got[i].prNum != w.pr {
			t.Errorf("entry %d = {%s #%s}, want {%s #%s}", i, got[i].repo, got[i].prNum, w.repo, w.pr)
		}
	}
}

func TestSelectablePRsDedupesAgentsPerPR(t *testing.T) {
	// Two agents on the same PR collapse to a single selectable entry, and the
	// entry carries the first agent's state.
	states := []*run.State{
		{ID: "first", Prompt: "p1", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/7"), CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "second", Prompt: "p2", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/7"), CreatedAt: "2026-01-01T00:01:00Z"},
	}
	got := selectablePRs(states)
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].state == nil || got[0].state.ID != "first" {
		t.Errorf("entry state = %v, want first agent's state", got[0].state)
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

func TestReconcileSelectionKeepsSelectedPR(t *testing.T) {
	prevStates := []*run.State{
		{ID: "a", Prompt: "p", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/1"), CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "b", Prompt: "p", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/2"), CreatedAt: "2026-01-01T00:01:00Z"},
		{ID: "c", Prompt: "p", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/3"), CreatedAt: "2026-01-01T00:02:00Z"},
	}
	m := dashboardModel{states: prevStates, cursor: 2} // selecting PR #3
	prev := selectablePRs(m.states)

	// PR #1 is removed; #3 still exists but shifts to index 1.
	m.states = []*run.State{prevStates[1], prevStates[2]}
	m.reconcileSelection(prev)

	if _, prNum, ok := m.selectedPR(); !ok || prNum != "3" {
		t.Errorf("after reload selection = %q (ok=%v), want PR #3", prNum, ok)
	}
}

func TestReconcileSelectionClampsWhenSelectedPRGone(t *testing.T) {
	prevStates := []*run.State{
		{ID: "a", Prompt: "p", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/1"), CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "b", Prompt: "p", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/2"), CreatedAt: "2026-01-01T00:01:00Z"},
	}
	m := dashboardModel{states: prevStates, cursor: 1} // selecting PR #2
	prev := selectablePRs(m.states)

	// PR #2 removed; only PR #1 remains. Cursor must clamp to a valid index.
	m.states = []*run.State{prevStates[0]}
	m.reconcileSelection(prev)

	if m.cursor != 0 {
		t.Errorf("cursor = %d, want clamped to 0", m.cursor)
	}
	if _, prNum, ok := m.selectedPR(); !ok || prNum != "1" {
		t.Errorf("selection = %q (ok=%v), want PR #1", prNum, ok)
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

	// Cursor on the second PR (#22). Approve via the same path the 'a' key uses.
	m := dashboardModel{store: store, states: states, cursor: 1}
	entries := selectablePRs(m.states)
	e := entries[clampCursor(m.cursor, len(entries))]
	if e.prNum != "22" {
		t.Fatalf("selected PR = %s, want 22", e.prNum)
	}
	if err := markApproved(e.state, m.store); err != nil {
		t.Fatalf("markApproved: %v", err)
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

package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/patflynn/klaus/internal/config"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/run"
)

// longSession returns runs resembling a long coordinator session (44 rows),
// and the expected on-screen order of the rows shown by default and with
// "show all". Each row is identified by its unique prompt, e.g. "attn 03".
//
//   - 4 active: three bare agents and a PR with a CI-fix agent running; one
//     started 5h ago and must never be hidden
//   - 7 needing attention, all over an hour old and cleaned up, never hidden
//   - 5 open PRs, one 3h old
//   - 21 merged PRs: 6 within the hour, 15 older (hidden)
//   - 6 exited agents whose worktrees were cleaned up, all old (hidden)
//   - 1 exited agent whose worktree is still there (kept: not cleaned up)
func longSession(now time.Time) (states []*run.State, shown, all []string) {
	id := 0
	add := func(s *run.State) *run.State {
		id++
		s.ID = fmt.Sprintf("20261004-1200-%04d", id)
		s.Type = "launch"
		s.TargetRepo = strPtr("testowner/testrepo")
		if s.CreatedAt == "" {
			s.CreatedAt = now.Format(time.RFC3339)
		}
		states = append(states, s)
		return s
	}
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	pr := func(n int) *string { return strPtr(fmt.Sprintf("https://github.com/testowner/testrepo/pull/%d", n)) }
	cost := float64Ptr(1.25)

	add(&run.State{Prompt: "active 01", TmuxPane: strPtr("%1"), CreatedAt: ago(2 * time.Minute), Worktree: "/wt"})
	add(&run.State{Prompt: "active 02", PRURL: pr(500), CostUSD: cost, CreatedAt: ago(3 * time.Hour)})
	add(&run.State{Prompt: "fix ci", PRURL: pr(500), TmuxPane: strPtr("%2"), CreatedAt: ago(5 * time.Minute)})
	add(&run.State{Prompt: "active 03", TmuxPane: strPtr("%3"), CreatedAt: ago(20 * time.Minute), Worktree: "/wt"})
	add(&run.State{Prompt: "active 04", TmuxPane: strPtr("%4"), CreatedAt: ago(5 * time.Hour), Worktree: "/wt"})
	for i := 1; i <= 7; i++ {
		add(&run.State{Prompt: fmt.Sprintf("attn %02d", i), CostUSD: cost, NeedsAttention: strPtr("agent/x"),
			CreatedAt: ago(time.Hour + time.Duration(i)*30*time.Minute)})
	}
	for i := 1; i <= 5; i++ {
		created := ago(time.Duration(i) * 10 * time.Minute)
		if i == 5 {
			created = ago(3 * time.Hour)
		}
		add(&run.State{Prompt: fmt.Sprintf("open %02d", i), PRURL: pr(600 + i), CostUSD: cost, CreatedAt: created, Worktree: "/wt"})
	}
	for i := 1; i <= 21; i++ {
		merged := time.Duration(i) * 5 * time.Minute // 5m..30m
		if i > 6 {
			merged = 2*time.Hour + time.Duration(i)*5*time.Minute // 2h35m..3h45m
		}
		add(&run.State{Prompt: fmt.Sprintf("merged %02d", i), PRURL: pr(700 + i), CostUSD: cost,
			CreatedAt: ago(merged + 30*time.Minute), MergedAt: strPtr(ago(merged))})
	}
	for i := 1; i <= 6; i++ {
		add(&run.State{Prompt: fmt.Sprintf("exited %02d", i), CostUSD: cost, CreatedAt: ago(4*time.Hour + time.Duration(i)*10*time.Minute)})
	}
	add(&run.State{Prompt: "kept 01", CostUSD: cost, CreatedAt: ago(6 * time.Hour), Worktree: "/wt/kept"})

	seq := func(prefix string, from, to int) []string {
		var out []string
		for i := from; i <= to; i++ {
			out = append(out, fmt.Sprintf("%s %02d", prefix, i))
		}
		return out
	}
	var head []string
	head = append(head, seq("active", 1, 4)...)
	head = append(head, seq("attn", 1, 7)...)
	head = append(head, seq("open", 1, 5)...)
	shown = append(append(append([]string{}, head...), seq("merged", 1, 6)...), "kept 01")
	all = append(append(append(append([]string{}, head...), seq("merged", 1, 21)...), seq("exited", 1, 6)...), "kept 01")
	return states, shown, all
}

func newLongSessionModel(now time.Time, width, height int) (dashboardModel, []string, []string) {
	states, shown, all := longSession(now)
	m := dashboardModel{
		states:    states,
		tmuxDeps:  testDashboardTmuxDeps(),
		ghStatus:  map[string]*prStatus{},
		hideAfter: defaultHideFinishedAfter,
	}
	m = send(m, tea.WindowSizeMsg{Width: width, Height: height})
	return m, shown, all
}

// send runs one message through Update.
func send(m dashboardModel, msg tea.Msg) dashboardModel {
	next, _ := m.Update(msg)
	return next.(dashboardModel)
}

func press(m dashboardModel, keys ...string) dashboardModel {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "pgup":
			msg = tea.KeyMsg{Type: tea.KeyPgUp}
		case "pgdown":
			msg = tea.KeyMsg{Type: tea.KeyPgDown}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m = send(m, msg)
	}
	return m
}

var promptRe = regexp.MustCompile(`\b(active|attn|open|merged|exited|kept) \d\d\b`)

// visibleRows returns the prompts of the rows visible in a rendered view, in
// order, and the prompt of the selected row.
func visibleRows(view string) (rows []string, selected string) {
	for _, line := range strings.Split(ansi.Strip(view), "\n") {
		p := promptRe.FindString(line)
		if p == "" {
			continue
		}
		rows = append(rows, p)
		if strings.HasPrefix(line, "> ") {
			selected = p
		}
	}
	return rows, selected
}

var positionRe = regexp.MustCompile(`\d+–\d+ of \d+`)

func position(view string) string {
	return positionRe.FindString(ansi.Strip(view))
}

// assertFits fails if the view has more lines than the pane is tall or any
// line is wider than the pane.
func assertFits(t *testing.T, view string, width, height int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		t.Errorf("%dx%d: view has %d lines, more than the pane height", width, height, len(lines))
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("%dx%d: line is %d wide: %q", width, height, w, ansi.Strip(l))
		}
	}
}

func TestDashboardLongListOrderingAndHiding(t *testing.T) {
	m, shown, all := newLongSessionModel(time.Now(), 160, 100)

	view := m.View()
	assertFits(t, view, 160, 100)
	rows, selected := visibleRows(view)
	if strings.Join(rows, ",") != strings.Join(shown, ",") {
		t.Errorf("visible rows:\n got %v\nwant %v", rows, shown)
	}
	if selected != "active 01" {
		t.Errorf("selected = %q, want the first row", selected)
	}
	plain := ansi.Strip(view)
	for _, want := range []string{
		"1–23 of 23", "21 hidden", "testowner/testrepo",
		"4 active · 7 attention · 5 open · 28 finished",
		"4/45 agents running", "agent:0003 running", "h show all",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("view missing %q:\n%s", want, plain)
		}
	}

	// "h" shows everything, in the same order, with nothing hidden.
	m = press(m, "h")
	view = m.View()
	rows, _ = visibleRows(view)
	if strings.Join(rows, ",") != strings.Join(all, ",") {
		t.Errorf("show-all rows:\n got %v\nwant %v", rows, all)
	}
	plain = ansi.Strip(view)
	if !strings.Contains(plain, "1–44 of 44") || strings.Contains(plain, "hidden") || !strings.Contains(plain, "h hide old") {
		t.Errorf("show-all header wrong:\n%s", plain)
	}

	// A negative threshold in config never hides anything.
	cfg := config.Config{Dashboard: &config.DashboardConfig{HideFinishedAfterMinutes: -1}}
	nm := newDashboardModel(run.NewGitDirStore(t.TempDir()), cfg, gh.NewGHCLIClient(""))
	nm.tmuxDeps = testDashboardTmuxDeps()
	nm.states = m.states
	if v := nm.resolve(time.Now()); v.hidden != 0 || len(v.rows) != 44 {
		t.Errorf("hide disabled: %d shown, %d hidden; want 44, 0", len(v.rows), v.hidden)
	}
}

func TestDashboardScrollsInSmallPane(t *testing.T) {
	m, shown, _ := newLongSessionModel(time.Now(), 60, 12)

	// 12 lines: header, source, summary, blank, 6 rows, blank, footer.
	steps := []struct {
		keys     []string
		pos      string
		selected string
	}{
		{nil, "1–6 of 23", shown[0]},
		{[]string{"k"}, "1–6 of 23", shown[0]}, // already at the top
		{[]string{"j", "j", "j", "j", "j", "j", "j", "j", "j", "j"}, "6–11 of 23", shown[10]},
		{[]string{"pgdown"}, "12–17 of 23", shown[16]},
		{[]string{"G"}, "18–23 of 23", shown[22]},
		{[]string{"j"}, "18–23 of 23", shown[22]}, // already at the bottom
		{[]string{"pgup"}, "12–17 of 23", shown[16]},
		{[]string{"g"}, "1–6 of 23", shown[0]},
	}
	for _, st := range steps {
		m = press(m, st.keys...)
		view := m.View()
		assertFits(t, view, 60, 12)
		rows, selected := visibleRows(view)
		if got := position(view); got != st.pos {
			t.Errorf("after %v: position %q, want %q", st.keys, got, st.pos)
		}
		if selected != st.selected {
			t.Errorf("after %v: selected %q, want %q (visible %v)", st.keys, selected, st.selected, rows)
		}
		if len(rows) != 6 {
			t.Errorf("after %v: %d rows visible, want 6", st.keys, len(rows))
		}
	}
}

func TestDashboardSelectionStaysVisibleWhenListChanges(t *testing.T) {
	now := time.Now()
	m, _, _ := newLongSessionModel(now, 60, 12)
	m = press(m, strings.Split(strings.Repeat("j", 13), "")...)
	if _, sel := visibleRows(m.View()); sel != "open 03" {
		t.Fatalf("selected %q, want open 03", sel)
	}

	// Five new agents start: they sort above the selection, which must stay
	// on "open 03" and scroll with it.
	states := append([]*run.State{}, m.states...)
	for i := 0; i < 5; i++ {
		states = append(states, &run.State{
			ID: fmt.Sprintf("20261004-1300-n%03d", i), Type: "launch", Prompt: fmt.Sprintf("active %02d", 50+i),
			TargetRepo: strPtr("testowner/testrepo"), TmuxPane: strPtr("%9"), CreatedAt: now.Format(time.RFC3339),
		})
	}
	m = send(m, statesLoadedMsg{states: states})
	view := m.View()
	if _, sel := visibleRows(view); sel != "open 03" {
		t.Errorf("after new runs: selected %q, want open 03 still visible:\n%s", sel, ansi.Strip(view))
	}
	if got := position(view); got != "14–19 of 28" {
		t.Errorf("after new runs: position %q, want 14–19 of 28", got)
	}

	// "open 03" merges: it moves to the finished group and stays selected.
	for _, s := range states {
		if s.Prompt == "open 03" {
			s.MergedAt = strPtr(now.Format(time.RFC3339))
		}
	}
	m = send(m, statesLoadedMsg{states: states})
	view = m.View()
	if _, sel := visibleRows(view); sel != "open 03" {
		t.Errorf("after merge: selected %q, want open 03 still visible:\n%s", sel, ansi.Strip(view))
	}
	assertFits(t, view, 60, 12)
}

func TestDashboardKeepsPRMergedWhileWatchedVisible(t *testing.T) {
	// "open 05" was created 3h ago. GitHub reports it merged, with no local
	// merge time. A dashboard that watched it while open keeps it for the
	// full threshold; one that only ever saw it merged hides it at once.
	now := time.Now()
	watching, _, _ := newLongSessionModel(now, 160, 100)
	watching.ghStatus["605"] = &prStatus{PRNumber: "605", State: "MERGED"}
	watching = send(watching, tickMsg{})

	fresh, _, _ := newLongSessionModel(now, 160, 100)
	fresh.ghStatus = watching.ghStatus
	fresh.seenUnfinished = nil
	fresh = send(fresh, tickMsg{})

	isShown := func(m dashboardModel) bool {
		rows, _ := visibleRows(m.View())
		for _, r := range rows {
			if r == "open 05" {
				return true
			}
		}
		return false
	}
	if !isShown(watching) {
		t.Error("PR merged while watched should stay visible")
	}
	if isShown(fresh) {
		t.Error("old PR first seen merged should be hidden")
	}
}

func TestDashboardNarrowPaneTruncatesColumnsNotRows(t *testing.T) {
	for _, width := range []int{30, 45} {
		m, shown, _ := newLongSessionModel(time.Now(), width, 40)
		view := m.View()
		assertFits(t, view, width, 40)
		n := 0
		for _, line := range strings.Split(ansi.Strip(view), "\n") {
			if regexp.MustCompile(`^(> |  )(#\d|agent:\d{4})`).MatchString(line) {
				n++
			}
		}
		if n != len(shown) {
			t.Errorf("width %d: %d rows visible, want all %d:\n%s", width, n, len(shown), ansi.Strip(view))
		}
		if !strings.Contains(ansi.Strip(view), "1–23 of 23") {
			t.Errorf("width %d: position indicator missing:\n%s", width, ansi.Strip(view))
		}
	}
}

func TestDashboardTinyPanesNeverPanic(t *testing.T) {
	for _, w := range []int{0, 1, 2, 5, 10, 20, 40} {
		for _, h := range []int{0, 1, 2, 3, 4, 5, 8} {
			m, _, _ := newLongSessionModel(time.Now(), w, h)
			m.recentErrors = []dashboardError{{Time: time.Now(), Message: "boom\nwith a newline"}}
			m.sandboxHosts = map[string]bool{"worker": true}
			for _, keys := range [][]string{nil, {"j", "j"}, {"G"}, {"pgdown"}, {"h"}, {"pgup", "k", "g"}} {
				m = press(m, keys...)
				view := m.View()
				if w == 0 || h == 0 {
					continue // size unknown: no limit to check
				}
				assertFits(t, view, w, h)
				if w >= 40 { // narrower panes drop the prompt column
					rows, _ := visibleRows(view)
					if want := min(h, 3); len(rows) < want {
						t.Errorf("%dx%d after %v: %d rows visible, want at least %d:\n%s", w, h, keys, len(rows), want, ansi.Strip(view))
					}
				}
			}
		}
	}
}

func TestDashboardEmptyAndAllHidden(t *testing.T) {
	m := dashboardModel{states: []*run.State{}, tmuxDeps: testDashboardTmuxDeps(), width: 80, height: 24, hideAfter: time.Hour}
	if view := ansi.Strip(m.View()); !strings.Contains(view, "No runs found") {
		t.Errorf("empty list should say so:\n%s", view)
	}

	old := time.Now().Add(-3 * time.Hour).Format(time.RFC3339)
	m.states = []*run.State{{ID: "20261004-1200-0001", Prompt: "merged", Type: "launch", CreatedAt: old, MergedAt: &old,
		PRURL: strPtr("https://github.com/o/r/pull/1")}}
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "1 finished hidden (h to show all)") || !strings.Contains(view, "1 hidden") {
		t.Errorf("all-hidden list should say how many are hidden:\n%s", view)
	}
}

func TestDashboardRepoColumnForMultipleRepos(t *testing.T) {
	now := time.Now().Format(time.RFC3339)
	m := dashboardModel{
		states: []*run.State{
			{ID: "20261004-1200-0001", Prompt: "one", Type: "launch", CreatedAt: now, TargetRepo: strPtr("testowner/alpha"), PRURL: strPtr("https://github.com/testowner/alpha/pull/1")},
			{ID: "20261004-1200-0002", Prompt: "two", Type: "launch", CreatedAt: now, TargetRepo: strPtr("testowner/beta"), PRURL: strPtr("https://github.com/testowner/beta/pull/2")},
		},
		tmuxDeps: testDashboardTmuxDeps(), width: 100, height: 24,
	}
	view := ansi.Strip(m.View())
	for _, want := range []string{"2 repos", "alpha  #1", "beta   #2"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}

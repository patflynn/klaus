package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
)

func TestGroupByRepo(t *testing.T) {
	states := []*run.State{
		{ID: "1", TargetRepo: strPtr("owner/repoA"), Type: "launch"},
		{ID: "2", TargetRepo: strPtr("owner/repoB"), Type: "launch"},
		{ID: "3", TargetRepo: strPtr("owner/repoA"), Type: "launch"},
		{ID: "4", Type: "session"}, // sessions should be excluded
	}

	groups := groupByRepo(states)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0].Repo != "owner/repoA" {
		t.Errorf("first group repo = %q, want %q", groups[0].Repo, "owner/repoA")
	}
	if len(groups[0].Runs) != 2 {
		t.Errorf("repoA runs = %d, want 2", len(groups[0].Runs))
	}
	if groups[1].Repo != "owner/repoB" {
		t.Errorf("second group repo = %q, want %q", groups[1].Repo, "owner/repoB")
	}
	if len(groups[1].Runs) != 1 {
		t.Errorf("repoB runs = %d, want 1", len(groups[1].Runs))
	}
}

func TestGroupByRepo_SessionsExcluded(t *testing.T) {
	states := []*run.State{
		{ID: "1", Type: "session"},
		{ID: "2", Type: "session"},
	}
	groups := groupByRepo(states)
	if len(groups) != 0 {
		t.Fatalf("expected 0 groups for session-only states, got %d", len(groups))
	}
}

func TestGroupByRepo_Empty(t *testing.T) {
	groups := groupByRepo(nil)
	if len(groups) != 0 {
		t.Fatalf("expected 0 groups for nil states, got %d", len(groups))
	}
}

func TestGroupByRepo_PRURLFallback(t *testing.T) {
	states := []*run.State{
		{ID: "1", Type: "launch", PRURL: strPtr("https://github.com/owner/repo/pull/42")},
	}
	groups := groupByRepo(states)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Repo != "owner/repo" {
		t.Errorf("repo = %q, want %q", groups[0].Repo, "owner/repo")
	}
}

func TestRepoFromPRURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/owner/repo/pull/42", "owner/repo"},
		{"https://github.com/pat/klaus/pull/1", "pat/klaus"},
		{"http://github.com/a/b/pull/99", "a/b"},
		{"notaurl", "(unknown)"},
		{"https://github.com/solo", "(unknown)"},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := repoFromPRURL(tt.url)
			if got != tt.want {
				t.Errorf("repoFromPRURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestRepoFromState(t *testing.T) {
	tests := []struct {
		name string
		s    *run.State
		want string
	}{
		{
			name: "uses TargetRepo when set",
			s:    &run.State{TargetRepo: strPtr("owner/repo")},
			want: "owner/repo",
		},
		{
			name: "falls back to PR URL",
			s:    &run.State{PRURL: strPtr("https://github.com/owner/repo2/pull/1")},
			want: "owner/repo2",
		},
		{
			name: "local when nothing set",
			s:    &run.State{},
			want: "(local)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repoFromState(tt.s, nil)
			if got != tt.want {
				t.Errorf("repoFromState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRepoFromState_WithRegistry(t *testing.T) {
	reg := &project.Registry{
		Projects: map[string]string{
			"cosmo": "/home/user/src/cosmo",
		},
	}

	tests := []struct {
		name string
		s    *run.State
		want string
	}{
		{
			name: "owner/repo normalizes to project name",
			s:    &run.State{TargetRepo: strPtr("patflynn/cosmo")},
			want: "cosmo",
		},
		{
			name: "project name stays as-is",
			s:    &run.State{TargetRepo: strPtr("cosmo")},
			want: "cosmo",
		},
		{
			name: "unregistered owner/repo stays as-is",
			s:    &run.State{TargetRepo: strPtr("patflynn/other")},
			want: "patflynn/other",
		},
		{
			name: "PRURL fallback normalizes to project name",
			s:    &run.State{PRURL: strPtr("https://github.com/patflynn/cosmo/pull/42")},
			want: "cosmo",
		},
		{
			name: "local when nothing set",
			s:    &run.State{},
			want: "(local)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repoFromState(tt.s, reg)
			if got != tt.want {
				t.Errorf("repoFromState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestComputeTotalCost(t *testing.T) {
	c1, c2 := 1.50, 3.25
	states := []*run.State{
		{CostUSD: &c1},
		{CostUSD: &c2},
		{}, // no cost
	}
	got := computeTotalCost(states)
	if got != 4.75 {
		t.Errorf("computeTotalCost() = %f, want 4.75", got)
	}
}

func TestComputeTotalCost_Empty(t *testing.T) {
	got := computeTotalCost(nil)
	if got != 0 {
		t.Errorf("computeTotalCost(nil) = %f, want 0", got)
	}
}

func TestCountAgents(t *testing.T) {
	states := []*run.State{
		{ID: "1", Type: "launch"},                                                           // not running (no pane)
		{ID: "2", Type: "launch", TmuxPane: strPtr("%2"), CostUSD: float64Ptr(1.0)},         // finished
		{ID: "3", Type: "watch", TmuxPane: strPtr("%3")},                                    // running
		{ID: "4", Type: "session"},                                                          // excluded
		{ID: "5", Type: "launch", TmuxPane: strPtr("%5"), DurationMS: int64Ptr(5000)},       // finished
		{ID: "6", Type: "launch", TmuxPane: strPtr("%6")},                                   // running
	}
	running, total := countAgents(states)
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if running != 2 {
		t.Errorf("running = %d, want 2", running)
	}
}

func TestIsAgentRunning(t *testing.T) {
	tests := []struct {
		name string
		s    *run.State
		want bool
	}{
		{"no pane", &run.State{}, false},
		{"pane but finalized with cost", &run.State{TmuxPane: strPtr("%1"), CostUSD: float64Ptr(1.0)}, false},
		{"pane but finalized with duration", &run.State{TmuxPane: strPtr("%1"), DurationMS: int64Ptr(1000)}, false},
		{"pane and not finalized", &run.State{TmuxPane: strPtr("%1")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAgentRunning(tt.s)
			if got != tt.want {
				t.Errorf("isAgentRunning() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestComputeSessionDuration(t *testing.T) {
	now := time.Now()
	past := now.Add(-2 * time.Hour)
	states := []*run.State{
		{CreatedAt: now.Format(time.RFC3339)},
		{CreatedAt: past.Format(time.RFC3339)},
	}
	dur := computeSessionDuration(states)
	if dur < 1*time.Hour {
		t.Errorf("duration = %v, want >= 1h", dur)
	}
}

func TestComputeSessionDuration_Empty(t *testing.T) {
	dur := computeSessionDuration(nil)
	if dur != 0 {
		t.Errorf("duration = %v, want 0", dur)
	}
}

func TestShortRunID(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"20260307-0913-3db3", "3db3"},
		{"ab", "ab"},
		{"abcd", "abcd"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := shortRunID(tt.id)
			if got != tt.want {
				t.Errorf("shortRunID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		dur  time.Duration
		want string
	}{
		{0, "0m"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h 30m"},
		{2*time.Hour + 5*time.Minute, "2h 05m"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatDuration(tt.dur)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.dur, got, tt.want)
			}
		})
	}
}

func TestPluralS(t *testing.T) {
	if pluralS(1) != "" {
		t.Error("pluralS(1) should be empty")
	}
	if pluralS(0) != "s" {
		t.Error("pluralS(0) should be 's'")
	}
	if pluralS(5) != "s" {
		t.Error("pluralS(5) should be 's'")
	}
}

func TestAgentStatusLabel(t *testing.T) {
	tests := []struct {
		name string
		s    *run.State
		want string
	}{
		{"with PR", &run.State{PRURL: strPtr("https://github.com/o/r/pull/1")}, "PR"},
		{"no PR", &run.State{}, "EXITED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agentStatusLabel(tt.s)
			if got != tt.want {
				t.Errorf("agentStatusLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClamp(t *testing.T) {
	tests := []struct {
		v, lo, hi, want int
	}{
		{5, 0, 10, 5},
		{-1, 0, 10, 0},
		{15, 0, 10, 10},
	}
	for _, tt := range tests {
		got := clamp(tt.v, tt.lo, tt.hi)
		if got != tt.want {
			t.Errorf("clamp(%d, %d, %d) = %d, want %d", tt.v, tt.lo, tt.hi, got, tt.want)
		}
	}
}

func TestLoadStatesFromDir(t *testing.T) {
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, "klaus", "runs")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write sample state files
	states := []run.State{
		{
			ID:        "20260307-0900-aaaa",
			Prompt:    "fix tests",
			Branch:    "fix-tests",
			Worktree:  "/tmp/worktree1",
			CreatedAt: "2026-03-07T09:00:00Z",
			Type:      "launch",
			PRURL:     strPtr("https://github.com/owner/repo/pull/30"),
		},
		{
			ID:        "20260307-0901-bbbb",
			Prompt:    "add feature",
			Branch:    "add-feature",
			Worktree:  "/tmp/worktree2",
			CreatedAt: "2026-03-07T09:01:00Z",
			Type:      "launch",
		},
	}

	for _, s := range states {
		data, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, s.ID+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store := run.NewGitDirStore(tmpDir)
	loaded, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d states, want 2", len(loaded))
	}

	// Verify newest first ordering
	if loaded[0].ID != "20260307-0901-bbbb" {
		t.Errorf("first state ID = %q, want newest first", loaded[0].ID)
	}
}

func TestDashboardViewRender(t *testing.T) {
	cost := 5.50
	// Use a repo name unlikely to be in any real registry to keep the test
	// independent of the user's ~/.klaus/projects.json.
	states := []*run.State{
		{
			ID:         "20260307-0900-aaaa",
			Prompt:     "tests requirement",
			Type:       "launch",
			CreatedAt:  time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
			TargetRepo: strPtr("testowner/testrepo"),
			PRURL:      strPtr("https://github.com/testowner/testrepo/pull/30"),
			CostUSD:    &cost,
		},
		{
			ID:         "20260307-0901-bbbb",
			Prompt:     "add dashboard",
			Type:       "launch",
			CreatedAt:  time.Now().Format(time.RFC3339),
			TargetRepo: strPtr("testowner/testrepo"),
			TmuxPane:   strPtr("%5"),
		},
	}

	m := dashboardModel{
		states:   states,
		ghStatus: map[string]*prStatus{},
		width:    80,
		height:   24,
	}

	view := m.View()

	// Verify key elements are present
	if !strings.Contains(view, "klaus dashboard") {
		t.Error("view should contain 'klaus dashboard' header")
	}
	if !strings.Contains(view, "testowner/testrepo") {
		t.Error("view should contain repo name")
	}
	if !strings.Contains(view, "#30") {
		t.Error("view should contain PR number")
	}
	if !strings.Contains(view, "bbbb") {
		t.Error("view should contain short run ID for bare agent")
	}
}

func TestDashboardViewNoRuns(t *testing.T) {
	m := dashboardModel{
		states:   []*run.State{},
		ghStatus: map[string]*prStatus{},
		width:    80,
		height:   24,
	}
	view := m.View()
	if !strings.Contains(view, "No runs found") {
		t.Error("empty state should show 'No runs found'")
	}
}

func TestDashboardViewLoading(t *testing.T) {
	m := dashboardModel{
		ghStatus: map[string]*prStatus{},
		width:    80,
		height:   24,
	}
	view := m.View()
	if !strings.Contains(view, "Loading") {
		t.Error("nil states should show 'Loading'")
	}
}

func TestStateLabel(t *testing.T) {
	tests := []struct {
		state string
		color string // just check the text content, not ANSI codes
	}{
		{"MERGED", "MERGED"},
		{"CLOSED", "CLOSED"},
		{"OPEN", "OPEN"},
		{"", "OPEN"},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			got := stateLabel(tt.state)
			if !strings.Contains(got, tt.color) {
				t.Errorf("stateLabel(%q) = %q, should contain %q", tt.state, got, tt.color)
			}
		})
	}
}

func TestCILabel(t *testing.T) {
	tests := []struct {
		ci   string
		want string
	}{
		{"passing", "CI ✓"},
		{"failing", "CI ✗"},
		{"pending", "CI …"},
		{"unknown", "CI ?"},
	}
	for _, tt := range tests {
		t.Run(tt.ci, func(t *testing.T) {
			got := ciLabel(tt.ci)
			if !strings.Contains(got, tt.want) {
				t.Errorf("ciLabel(%q) = %q, should contain %q", tt.ci, got, tt.want)
			}
		})
	}
}

func TestRenderPRLine(t *testing.T) {
	m := dashboardModel{width: 80, ghStatus: map[string]*prStatus{}}
	s := &run.State{
		ID:     "20260307-0900-aaaa",
		Prompt: "fix bug",
		Type:   "launch",
		PRURL:  strPtr("https://github.com/o/r/pull/42"),
	}

	// Without GitHub status
	line := m.renderPRLine("42", s, nil)
	if !strings.Contains(line, "#42") {
		t.Error("should contain PR number")
	}
	if !strings.Contains(line, "fix bug") {
		t.Error("should contain prompt")
	}
	if !strings.Contains(line, "OPEN") {
		t.Error("should show OPEN by default")
	}

	// With GitHub status showing passing CI and conflicts
	ps := &prStatus{
		PRNumber:  "42",
		State:     "OPEN",
		CI:        "passing",
		Conflicts: "yes",
	}
	line = m.renderPRLine("42", s, ps)
	if !strings.Contains(line, "CI ✓") {
		t.Error("should show passing CI")
	}
	if !strings.Contains(line, "conflicts") {
		t.Error("should show conflicts")
	}

	// Merged PR
	ps.State = "MERGED"
	line = m.renderPRLine("42", s, ps)
	if !strings.Contains(line, "MERGED") {
		t.Error("should show MERGED")
	}
}

func TestRenderGroupCounts(t *testing.T) {
	m := dashboardModel{width: 80, ghStatus: map[string]*prStatus{}}
	g := repoGroup{
		Repo: "owner/repo",
		Runs: []*run.State{
			{ID: "1", Type: "launch", PRURL: strPtr("https://github.com/o/r/pull/1")},
			{ID: "2", Type: "launch", PRURL: strPtr("https://github.com/o/r/pull/2")},
			{ID: "3", Type: "launch"},
		},
		PRMap: map[string]*prStatus{},
	}

	rendered := m.renderGroup(g)
	if !strings.Contains(rendered, "3 agents") {
		t.Errorf("should show '3 agents', got: %s", rendered)
	}
	if !strings.Contains(rendered, "2 PRs") {
		t.Errorf("should show '2 PRs', got: %s", rendered)
	}
}

func TestRightAlignPad(t *testing.T) {
	got := rightAlignPad("hello", 10)
	if len(got) != 10 {
		t.Errorf("rightAlignPad length = %d, want 10", len(got))
	}
	if !strings.HasSuffix(got, "hello") {
		t.Errorf("rightAlignPad should end with 'hello', got %q", got)
	}

	// When string is wider than total
	got = rightAlignPad("hello", 3)
	if got != "hello" {
		t.Errorf("rightAlignPad with narrow width should return original, got %q", got)
	}
}

// Helper functions for tests.

func float64Ptr(f float64) *float64 {
	return &f
}

func int64Ptr(i int64) *int64 {
	return &i
}

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
)

func TestIsBudgetExhausted(t *testing.T) {
	budget := "5.00"
	cost1 := 4.99
	cost2 := 4.74
	cost3 := 0.01

	tests := []struct {
		name  string
		state *run.State
		want  bool
	}{
		{
			name:  "cost above 95% of budget",
			state: &run.State{Budget: &budget, CostUSD: &cost1},
			want:  true,
		},
		{
			name:  "cost below 95% of budget",
			state: &run.State{Budget: &budget, CostUSD: &cost2},
			want:  false,
		},
		{
			name:  "tiny cost",
			state: &run.State{Budget: &budget, CostUSD: &cost3},
			want:  false,
		},
		{
			name:  "no budget set",
			state: &run.State{CostUSD: &cost1},
			want:  false,
		},
		{
			name:  "no cost set",
			state: &run.State{Budget: &budget},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBudgetExhausted(tt.state); got != tt.want {
				t.Errorf("isBudgetExhausted() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPauseAfterBudgetExhaustion(t *testing.T) {
	dir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(dir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	worktreePath := filepath.Join(dir, "wt")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("creating fake worktree: %v", err)
	}

	budget := "1.00"
	cost := 1.05
	pane := "%42"
	state := &run.State{
		ID:       "test-run",
		Branch:   "agent/test",
		Worktree: worktreePath,
		TmuxPane: &pane,
		Budget:   &budget,
		CostUSD:  &cost,
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	pauseAfterBudgetExhaustion(store, state)

	// State updated in place
	if state.Status == nil || *state.Status != run.StatusPaused {
		t.Errorf("expected status=paused, got %v", state.Status)
	}
	if state.PauseReason == nil || *state.PauseReason != run.PauseReasonBudgetExceeded {
		t.Errorf("expected PauseReason=%q, got %v", run.PauseReasonBudgetExceeded, state.PauseReason)
	}
	if state.PausedAt == nil || *state.PausedAt == "" {
		t.Errorf("expected PausedAt to be set")
	}
	// Worktree and pane MUST be preserved
	if state.Worktree == "" {
		t.Errorf("expected worktree to be preserved on pause")
	}
	if state.TmuxPane == nil {
		t.Errorf("expected TmuxPane to be preserved on pause")
	}
	// Disk reflects same
	saved, err := store.Load("test-run")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Status == nil || *saved.Status != run.StatusPaused {
		t.Errorf("expected saved status=paused, got %v", saved.Status)
	}

	// Worktree directory still present
	if _, err := os.Stat(worktreePath); err != nil {
		t.Errorf("expected worktree directory to still exist: %v", err)
	}

	// agent:paused event emitted
	log := event.NewLog(dir)
	events, err := log.Read()
	if err != nil {
		t.Fatalf("reading events: %v", err)
	}
	found := false
	for _, evt := range events {
		if evt.Type == event.AgentPaused && evt.RunID == "test-run" {
			found = true
			if reason, _ := evt.Data["reason"].(string); reason != run.PauseReasonBudgetExceeded {
				t.Errorf("paused event reason = %q, want %q", reason, run.PauseReasonBudgetExceeded)
			}
		}
	}
	if !found {
		t.Errorf("expected agent:paused event in log, got events: %+v", events)
	}
}

func TestCleanupOneAllowsPausedRuns(t *testing.T) {
	// A paused run keeps its tmux pane alive — the cleanup deps must not
	// classify that as "active" or 'klaus cleanup' would skip it without
	// --force.
	paused := run.StatusPaused
	pane := "%42"
	state := &run.State{
		ID:       "paused-run",
		Status:   &paused,
		TmuxPane: &pane,
	}

	// Stub a tmux client that reports the pane as alive but not dead.
	tc := &noopTmux{paneAlive: true}
	if defaultIsRunActive(state, tc) {
		t.Errorf("expected paused run with live pane to NOT be classified active")
	}
}

func TestCleanupOneAllowsFinalizedRuns(t *testing.T) {
	finalized := run.StatusFinalized
	state := &run.State{
		ID:     "finalized-run",
		Status: &finalized,
	}
	tc := &noopTmux{paneAlive: true}
	if defaultIsRunActive(state, tc) {
		t.Errorf("expected finalized run to NOT be classified active")
	}
}

// noopTmux satisfies tmux.Client for the cleanup-active tests. It embeds
// the real ExecClient so the methods we don't care about compile; tests
// here only call PaneExists, which is overridden below.
type noopTmux struct {
	tmux.ExecClient
	paneAlive bool
}

func (n *noopTmux) PaneExists(_ context.Context, _ string) bool { return n.paneAlive }

func TestComputeResumeBudget(t *testing.T) {
	five := "5.00"
	ten := "10.00"

	tests := []struct {
		name           string
		current        *string
		addBudget      float64
		absoluteBudget string
		want           string
		wantErr        bool
	}{
		{
			name:    "default add when both flags unset",
			current: &five,
			want:    "10.00",
		},
		{
			name:      "explicit add",
			current:   &five,
			addBudget: 3,
			want:      "8.00",
		},
		{
			name:           "absolute overrides add",
			current:        &five,
			addBudget:      3,
			absoluteBudget: "20",
			want:           "20",
		},
		{
			name:    "nil current treated as zero",
			current: nil,
			want:    "5.00",
		},
		{
			name:           "invalid absolute is rejected",
			current:        &ten,
			absoluteBudget: "not-a-number",
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeResumeBudget(tt.current, tt.addBudget, tt.absoluteBudget)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("computeResumeBudget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatusOrRunning(t *testing.T) {
	if got := statusOrRunning(""); got != "running" {
		t.Errorf("expected 'running' for empty status, got %q", got)
	}
	if got := statusOrRunning("paused"); got != "paused" {
		t.Errorf("expected 'paused', got %q", got)
	}
}

func TestPromptTitle(t *testing.T) {
	long := strings.Repeat("a", 200)
	tests := []struct {
		in     string
		minLen int
		maxLen int
	}{
		{in: "Fix the auth bug", minLen: 1, maxLen: 70},
		{in: long, minLen: 70, maxLen: 70},
		{in: "", minLen: 1, maxLen: 80},
	}
	for _, tt := range tests {
		got := promptTitle(tt.in)
		if len(got) < tt.minLen || len(got) > tt.maxLen {
			t.Errorf("promptTitle(%q) length %d outside [%d,%d]", tt.in, len(got), tt.minLen, tt.maxLen)
		}
	}
}

package pipeline

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
)

func newTestController(t *testing.T) (*Controller, string) {
	t.Helper()
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "session")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	eventLog := event.NewLog(baseDir)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	stateDir := filepath.Join(dir, "klaus")
	store := run.NewGitDirStore(stateDir)

	c := New(store, eventLog, logger)
	return c, dir
}

func TestStateTransition_CIPendingToFailed(t *testing.T) {
	c, _ := newTestController(t)

	var launchedPR string
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchedPR = prNumber
		return "agent-001", nil
	})

	statuses := map[string]*PRStatus{
		"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
	}

	actions := c.HandleGHStatus(context.Background(), statuses, nil)

	if launchedPR != "42" {
		t.Errorf("expected agent launched for PR #42, got %q", launchedPR)
	}
	if len(actions) != 1 || actions[0].Type != "launch" {
		t.Errorf("expected 1 launch action, got %v", actions)
	}

	states := c.PipelineStates()
	if states["42"].Stage != StageCIFailed {
		t.Errorf("expected stage ci_failed, got %s", states["42"].Stage)
	}
}

func TestStateTransition_CIFailedToPassedToApproved(t *testing.T) {
	c, _ := newTestController(t)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return "agent-001", nil
	})
	c.SetMergePRs(func(ctx context.Context, repo string, prNumbers []string) error {
		return nil
	})

	// Step 1: CI failing -> dispatches agent
	statuses := map[string]*PRStatus{
		"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
	}
	c.HandleGHStatus(context.Background(), statuses, nil)

	if c.PipelineStates()["42"].Stage != StageCIFailed {
		t.Fatalf("expected ci_failed, got %s", c.PipelineStates()["42"].Stage)
	}

	// Step 2: CI passes (no review yet)
	statuses["42"] = &PRStatus{PRNumber: "42", State: "OPEN", CI: "passing", TargetRepo: "owner/repo"}
	c.HandleGHStatus(context.Background(), statuses, nil)

	if c.PipelineStates()["42"].Stage != StageCIPassed {
		t.Fatalf("expected ci_passed, got %s", c.PipelineStates()["42"].Stage)
	}

	// Step 3: Approved -> auto-merge
	statuses["42"] = &PRStatus{
		PRNumber: "42", State: "OPEN", CI: "passing",
		ReviewDecision: "APPROVED", Conflicts: "none", TargetRepo: "owner/repo",
	}
	actions := c.HandleGHStatus(context.Background(), statuses, nil)

	// Should have merged
	hasMerge := false
	for _, a := range actions {
		if a.Type == "merge" {
			hasMerge = true
		}
	}
	if !hasMerge {
		t.Error("expected merge action after approval")
	}
}

func TestNoDuplicateAgentDispatch(t *testing.T) {
	c, _ := newTestController(t)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return "agent-001", nil
	})

	statuses := map[string]*PRStatus{
		"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
	}

	// First call should dispatch.
	c.HandleGHStatus(context.Background(), statuses, nil)
	if launchCount != 1 {
		t.Fatalf("expected 1 launch, got %d", launchCount)
	}

	// Simulate agent still running.
	runStates := []*run.State{
		{ID: "agent-001", TmuxPane: strPtr("%1")}, // running: has pane, no cost/duration
	}
	c.HandleGHStatus(context.Background(), statuses, runStates)
	if launchCount != 1 {
		t.Errorf("expected no duplicate launch, got %d total", launchCount)
	}
}

func TestAgentReDispatchAfterCompletion(t *testing.T) {
	c, _ := newTestController(t)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return "agent-002", nil
	})

	statuses := map[string]*PRStatus{
		"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
	}

	// First dispatch.
	c.HandleGHStatus(context.Background(), statuses, nil)
	if launchCount != 1 {
		t.Fatalf("expected 1 launch, got %d", launchCount)
	}

	// Agent completed (finalized with cost).
	cost := 1.0
	runStates := []*run.State{
		{ID: "agent-002", TmuxPane: strPtr("%1"), CostUSD: &cost},
	}

	// CI still failing -> should re-dispatch.
	c.HandleGHStatus(context.Background(), statuses, runStates)
	if launchCount != 2 {
		t.Errorf("expected re-dispatch after agent completed, got %d total", launchCount)
	}
}

func TestReviewCommentsDispatchAgent(t *testing.T) {
	c, _ := newTestController(t)

	var launchedPrompt string
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchedPrompt = prompt
		return "agent-review", nil
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "CHANGES_REQUESTED", TargetRepo: "owner/repo",
		},
	}

	actions := c.HandleGHStatus(context.Background(), statuses, nil)

	if launchedPrompt == "" {
		t.Error("expected agent dispatch for review comments")
	}
	if len(actions) != 1 || actions[0].Type != "launch" {
		t.Errorf("expected 1 launch action, got %v", actions)
	}

	states := c.PipelineStates()
	if states["42"].Stage != StageReviewPending {
		t.Errorf("expected stage review_pending, got %s", states["42"].Stage)
	}
}

func TestMergedPRCleanedUp(t *testing.T) {
	c, _ := newTestController(t)
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		return "agent-001", nil
	})

	// Start with a CI failing PR.
	statuses := map[string]*PRStatus{
		"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
	}
	c.HandleGHStatus(context.Background(), statuses, nil)

	if len(c.PipelineStates()) != 1 {
		t.Fatal("expected 1 tracked PR")
	}

	// PR gets merged externally.
	statuses["42"] = &PRStatus{PRNumber: "42", State: "MERGED"}
	c.HandleGHStatus(context.Background(), statuses, nil)

	if len(c.PipelineStates()) != 0 {
		t.Error("expected merged PR to be cleaned up from tracking")
	}
}

func TestClosedPRCleanedUp(t *testing.T) {
	c, _ := newTestController(t)

	// Manually seed state.
	c.mu.Lock()
	c.prStates["99"] = &PRPipelineState{PRNumber: "99", Stage: StageCIPassed}
	c.mu.Unlock()

	statuses := map[string]*PRStatus{
		"99": {PRNumber: "99", State: "CLOSED"},
	}
	c.HandleGHStatus(context.Background(), statuses, nil)

	if len(c.PipelineStates()) != 0 {
		t.Error("expected closed PR to be cleaned up from tracking")
	}
}

func TestAutoMergeBlockedByConflicts(t *testing.T) {
	c, _ := newTestController(t)

	mergeCount := 0
	c.SetMergePRs(func(ctx context.Context, repo string, prNumbers []string) error {
		mergeCount++
		return nil
	})
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		return "agent-001", nil
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "APPROVED", Conflicts: "yes", TargetRepo: "owner/repo",
		},
	}

	c.HandleGHStatus(context.Background(), statuses, nil)

	if mergeCount != 0 {
		t.Error("should not merge when conflicts exist")
	}

	states := c.PipelineStates()
	if states["42"].Stage != StageApproved {
		t.Errorf("expected stage approved (blocked by conflicts), got %s", states["42"].Stage)
	}
}

func TestStageLabelCoverage(t *testing.T) {
	tests := []struct {
		stage Stage
		want  string
	}{
		{StageCIPending, "CI pending"},
		{StageCIFailed, "CI failed, fix running"},
		{StageCIPassed, "CI passed, reviewing"},
		{StageReviewPending, "review fix running"},
		{StageApproved, "approved, ready"},
		{StageMerging, "merging"},
		{StageMerged, "merged"},
		{StageStalled, "stalled"},
		{Stage("unknown"), "unknown"},
	}
	for _, tt := range tests {
		got := StageLabel(tt.stage)
		if got != tt.want {
			t.Errorf("StageLabel(%q) = %q, want %q", tt.stage, got, tt.want)
		}
	}
}

func TestExtractAgentID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Launching agent 20260401-1500-abc1...\n  worktree: ...", "20260401-1500-abc1"},
		{"Launching agent abc123 (PR #42 fix)...", "abc123"},
		{"no match here", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractAgentID(tt.input)
		if got != tt.want {
			t.Errorf("extractAgentID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func strPtr(s string) *string {
	return &s
}

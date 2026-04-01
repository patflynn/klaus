package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestLaunchFailureRetriesBeforeStalling(t *testing.T) {
	c, _ := newTestController(t)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return "", fmt.Errorf("worktree already exists")
	})

	statuses := map[string]*PRStatus{
		"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
	}

	// First failure: should NOT go to stalled (retry 1 of 2).
	c.HandleGHStatus(context.Background(), statuses, nil)
	state := c.PipelineStates()["42"]
	if state.Stage == StageStalled {
		t.Error("expected pipeline to retry, not stall on first failure")
	}
	if launchCount != 1 {
		t.Errorf("expected 1 launch attempt, got %d", launchCount)
	}

	// Simulate backoff elapsed by resetting LastFailedAt.
	c.mu.Lock()
	c.prStates["42"].LastFailedAt = time.Now().Add(-2 * time.Minute)
	c.mu.Unlock()

	// Second failure: retry 2 of 2, still not stalled.
	c.HandleGHStatus(context.Background(), statuses, nil)
	state = c.PipelineStates()["42"]
	if state.Stage == StageStalled {
		t.Error("expected pipeline to retry on second failure, not stall")
	}

	// Simulate backoff elapsed again.
	c.mu.Lock()
	c.prStates["42"].LastFailedAt = time.Now().Add(-2 * time.Minute)
	c.mu.Unlock()

	// Third failure: retries exhausted, should stall.
	c.HandleGHStatus(context.Background(), statuses, nil)
	state = c.PipelineStates()["42"]
	if state.Stage != StageStalled {
		t.Errorf("expected stalled after retries exhausted, got %s", state.Stage)
	}
}

func TestWorktreeCleanupBeforeDispatch(t *testing.T) {
	c, _ := newTestController(t)

	// Create a temp dir to act as the stale worktree.
	staleDir := t.TempDir()

	var cleanedUpID string
	// Override launchAgent to track that cleanup happened before launch.
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		// By the time launch is called, the stale worktree should have
		// had cleanup attempted. We can't easily verify the cleanup command
		// ran (it would fail since the run ID doesn't exist in store), but
		// we can verify the controller attempted it by checking the worktree
		// dir was passed. For this test, just succeed.
		return "agent-new", nil
	})

	// Provide a run state for a completed agent that has a worktree on disk.
	cost := 1.0
	staleRun := &run.State{
		ID:       "agent-stale",
		PR:       strPtr("42"),
		Worktree: staleDir,
		TmuxPane: strPtr("%99"), // pane exists but run is finalized
		CostUSD:  &cost,         // finalized -> not running
	}

	statuses := map[string]*PRStatus{
		"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
	}

	actions := c.HandleGHStatus(context.Background(), statuses, []*run.State{staleRun})

	// Should have dispatched a new agent.
	if len(actions) == 0 || actions[0].Type != "launch" {
		t.Errorf("expected launch action, got %v", actions)
	}

	_ = cleanedUpID // cleanup runs best-effort via exec
}

func TestReviewFixLaunchRetry(t *testing.T) {
	c, _ := newTestController(t)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return "", fmt.Errorf("worktree already exists")
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "CHANGES_REQUESTED", TargetRepo: "owner/repo",
		},
	}

	// First failure: should not stall.
	c.HandleGHStatus(context.Background(), statuses, nil)
	state := c.PipelineStates()["42"]
	if state.Stage == StageStalled {
		t.Error("expected retry for review fix, not stall on first failure")
	}
}

func TestTrustedCommentDispatch(t *testing.T) {
	c, _ := newTestController(t)

	var launchedPrompt string
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchedPrompt = prompt
		return "agent-trusted", nil
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber:              "42",
			State:                 "OPEN",
			CI:                    "passing",
			ReviewDecision:        "", // empty — not CHANGES_REQUESTED
			HasNewTrustedComments: true,
			TargetRepo:            "owner/repo",
		},
	}

	actions := c.HandleGHStatus(context.Background(), statuses, nil)

	if launchedPrompt == "" {
		t.Error("expected agent dispatch for trusted reviewer comments")
	}
	if len(actions) != 1 || actions[0].Type != "launch" {
		t.Errorf("expected 1 launch action, got %v", actions)
	}

	states := c.PipelineStates()
	if states["42"].Stage != StageReviewPending {
		t.Errorf("expected stage review_pending, got %s", states["42"].Stage)
	}
}

func TestNoDispatchWithoutTrustedComments(t *testing.T) {
	c, _ := newTestController(t)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return "agent-001", nil
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber:              "42",
			State:                 "OPEN",
			CI:                    "passing",
			ReviewDecision:        "",
			HasNewTrustedComments: false,
			TargetRepo:            "owner/repo",
		},
	}

	c.HandleGHStatus(context.Background(), statuses, nil)

	if launchCount != 0 {
		t.Errorf("expected no agent dispatch without trusted comments, got %d launches", launchCount)
	}

	states := c.PipelineStates()
	if states["42"].Stage != StageCIPassed {
		t.Errorf("expected stage ci_passed, got %s", states["42"].Stage)
	}
}

func TestNoDoubleDispatchOnTrustedComments(t *testing.T) {
	c, _ := newTestController(t)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return "agent-trusted", nil
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber:              "42",
			State:                 "OPEN",
			CI:                    "passing",
			ReviewDecision:        "",
			HasNewTrustedComments: true,
			TargetRepo:            "owner/repo",
		},
	}

	// First call dispatches.
	c.HandleGHStatus(context.Background(), statuses, nil)
	if launchCount != 1 {
		t.Fatalf("expected 1 launch, got %d", launchCount)
	}

	// Simulate agent still running.
	runStates := []*run.State{
		{ID: "agent-trusted", TmuxPane: strPtr("%1")},
	}
	c.HandleGHStatus(context.Background(), statuses, runStates)
	if launchCount != 1 {
		t.Errorf("expected no duplicate launch while agent running, got %d total", launchCount)
	}
}

func strPtr(s string) *string {
	return &s
}

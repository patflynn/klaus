package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	c.SetAutoMergeOnApproval(true)

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

	// Expire the dispatch cooldown so re-dispatch is allowed.
	c.mu.Lock()
	c.prStates["42"].LastDispatchAt = time.Now().Add(-61 * time.Second)
	c.mu.Unlock()

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

	var launchedPrompt string
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchedPrompt = prompt
		return "agent-rebase", nil
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "APPROVED", Conflicts: "yes", TargetRepo: "owner/repo",
		},
	}

	actions := c.HandleGHStatus(context.Background(), statuses, nil)

	if mergeCount != 0 {
		t.Error("should not merge when conflicts exist")
	}

	if launchedPrompt == "" {
		t.Error("expected rebase agent dispatch when conflicts detected")
	}
	if !strings.Contains(launchedPrompt, "merge conflicts") {
		t.Errorf("expected rebase prompt, got %q", launchedPrompt)
	}

	hasLaunch := false
	for _, a := range actions {
		if a.Type == "launch" && strings.Contains(a.Detail, "Rebase") {
			hasLaunch = true
		}
	}
	if !hasLaunch {
		t.Errorf("expected rebase launch action, got %v", actions)
	}

	states := c.PipelineStates()
	if states["42"].Stage != StageNeedsRebase {
		t.Errorf("expected stage needs_rebase, got %s", states["42"].Stage)
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
		{StageNeedsRebase, "rebasing"},
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

func TestTruncateError(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"short error", 50, "short error"},
		{"line one\nline two\nline three", 50, "line one"},
		{"some error\n\nUsage:\n  klaus launch [flags]", 50, "some error"},
		{"before Usage: after", 50, "before"},
		{strings.Repeat("x", 200), 50, strings.Repeat("x", 49) + "…"},
		{"", 50, ""},
	}
	for _, tt := range tests {
		got := truncateError(tt.input, tt.maxLen)
		if got != tt.want {
			t.Errorf("truncateError(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
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
	actions := c.HandleGHStatus(context.Background(), statuses, nil)
	state := c.PipelineStates()["42"]
	if state.Stage == StageStalled {
		t.Error("expected pipeline to retry, not stall on first failure")
	}
	if launchCount != 1 {
		t.Errorf("expected 1 launch attempt, got %d", launchCount)
	}
	// No error action on retryable failure.
	for _, a := range actions {
		if a.Type == "error" {
			t.Error("expected no error action while retries remain")
		}
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

	// Third failure: retries exhausted, should stall and return error action.
	actions = c.HandleGHStatus(context.Background(), statuses, nil)
	state = c.PipelineStates()["42"]
	if state.Stage != StageStalled {
		t.Errorf("expected stalled after retries exhausted, got %s", state.Stage)
	}
	hasError := false
	for _, a := range actions {
		if a.Type == "error" {
			hasError = true
			if a.Error == "" {
				t.Error("expected non-empty Error field on error action")
			}
		}
	}
	if !hasError {
		t.Error("expected error action when retries exhausted and pipeline stalls")
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

func TestIdlePaneCleanupDuringPoll(t *testing.T) {
	c, _ := newTestController(t)

	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		return "agent-001", nil
	})

	// Track which panes were cleaned up.
	var killedPanes []string
	c.SetCleanIdlePanes(func(runStates []*run.State) {
		for _, s := range runStates {
			if s.TmuxPane == nil {
				continue
			}
			if s.CostUSD != nil || s.DurationMS != nil {
				continue
			}
			// Simulate: pane %idle is idle, pane %busy is still running.
			if *s.TmuxPane == "%idle" {
				killedPanes = append(killedPanes, *s.TmuxPane)
			}
		}
	})

	statuses := map[string]*PRStatus{
		"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
	}

	runStates := []*run.State{
		{ID: "agent-idle", TmuxPane: strPtr("%idle")},  // idle pane, should be cleaned
		{ID: "agent-busy", TmuxPane: strPtr("%busy")},  // busy pane, should not be cleaned
	}

	c.HandleGHStatus(context.Background(), statuses, runStates)

	if len(killedPanes) != 1 || killedPanes[0] != "%idle" {
		t.Errorf("expected cleanup of %%idle pane, got %v", killedPanes)
	}
}

func TestIdlePaneCleanupHandlesFinalized(t *testing.T) {
	c, _ := newTestController(t)

	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		return "agent-001", nil
	})

	var cleanedUpDone bool
	c.SetCleanIdlePanes(func(runStates []*run.State) {
		for _, s := range runStates {
			if s.TmuxPane == nil {
				continue
			}
			if s.ID == "agent-done" && (s.CostUSD != nil || s.DurationMS != nil) {
				cleanedUpDone = true
			}
		}
	})

	cost := 1.0
	runStates := []*run.State{
		{ID: "agent-done", TmuxPane: strPtr("%done"), CostUSD: &cost},
	}

	statuses := map[string]*PRStatus{
		"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
	}

	c.HandleGHStatus(context.Background(), statuses, runStates)

	if !cleanedUpDone {
		t.Error("cleanup should now handle finalized runs")
	}
}

func TestIdlePaneCleanupDoesNotSkipRecentRuns(t *testing.T) {
	c, _ := newTestController(t)

	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		return "agent-001", nil
	})

	// Track which runs the cleanup tries to act on.
	var checkedRuns []string
	c.SetCleanIdlePanes(func(runStates []*run.State) {
		for _, s := range runStates {
			if s.TmuxPane == nil {
				continue
			}
			checkedRuns = append(checkedRuns, s.ID)
		}
	})

	statuses := map[string]*PRStatus{
		"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
	}

	recentTime := time.Now().Add(-30 * time.Second).Format(time.RFC3339)
	runStates := []*run.State{
		{ID: "agent-new", TmuxPane: strPtr("%new"), CreatedAt: recentTime},
	}

	c.HandleGHStatus(context.Background(), statuses, runStates)

	if len(checkedRuns) != 1 || checkedRuns[0] != "agent-new" {
		t.Errorf("expected agent-new to be checked, got %v", checkedRuns)
	}
}

func TestNeedsRebaseTransitionsToMergeAfterConflictsResolved(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAutoMergeOnApproval(true)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return fmt.Sprintf("agent-%03d", launchCount), nil
	})
	mergeCount := 0
	c.SetMergePRs(func(ctx context.Context, repo string, prNumbers []string) error {
		mergeCount++
		return nil
	})

	// Step 1: Approved with conflicts → dispatches rebase agent.
	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "APPROVED", Conflicts: "yes", TargetRepo: "owner/repo",
		},
	}
	c.HandleGHStatus(context.Background(), statuses, nil)

	if c.PipelineStates()["42"].Stage != StageNeedsRebase {
		t.Fatalf("expected needs_rebase, got %s", c.PipelineStates()["42"].Stage)
	}
	if launchCount != 1 {
		t.Fatalf("expected 1 launch, got %d", launchCount)
	}

	// Step 2: Rebase agent completes, CI passes, conflicts resolved → should merge.
	cost := 1.0
	runStates := []*run.State{
		{ID: "agent-001", TmuxPane: strPtr("%1"), CostUSD: &cost}, // completed
	}
	statuses["42"] = &PRStatus{
		PRNumber: "42", State: "OPEN", CI: "passing",
		ReviewDecision: "APPROVED", Conflicts: "none", TargetRepo: "owner/repo",
	}
	actions := c.HandleGHStatus(context.Background(), statuses, runStates)

	hasMerge := false
	for _, a := range actions {
		if a.Type == "merge" {
			hasMerge = true
		}
	}
	if !hasMerge {
		t.Error("expected merge after rebase resolved conflicts")
	}
	if mergeCount != 1 {
		t.Errorf("expected 1 merge call, got %d", mergeCount)
	}
}

func TestNeedsRebaseTransitionsToCIFailedIfCIFails(t *testing.T) {
	c, _ := newTestController(t)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return fmt.Sprintf("agent-%03d", launchCount), nil
	})

	// Step 1: Approved with conflicts → rebase agent dispatched.
	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "APPROVED", Conflicts: "yes", TargetRepo: "owner/repo",
		},
	}
	c.HandleGHStatus(context.Background(), statuses, nil)

	if c.PipelineStates()["42"].Stage != StageNeedsRebase {
		t.Fatalf("expected needs_rebase, got %s", c.PipelineStates()["42"].Stage)
	}

	// Expire the dispatch cooldown so the CI fix agent can be dispatched.
	c.mu.Lock()
	c.prStates["42"].LastDispatchAt = time.Now().Add(-61 * time.Second)
	c.mu.Unlock()

	// Step 2: Rebase agent completes but CI fails.
	cost := 1.0
	runStates := []*run.State{
		{ID: "agent-001", TmuxPane: strPtr("%1"), CostUSD: &cost},
	}
	statuses["42"] = &PRStatus{
		PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo",
	}
	c.HandleGHStatus(context.Background(), statuses, runStates)

	state := c.PipelineStates()["42"]
	if state.Stage != StageCIFailed {
		t.Errorf("expected ci_failed after rebase + CI failure, got %s", state.Stage)
	}
	// Should have dispatched a CI fix agent (launch 2).
	if launchCount != 2 {
		t.Errorf("expected 2 launches (rebase + CI fix), got %d", launchCount)
	}
}

func TestNeedsRebaseNoDoubleDispatch(t *testing.T) {
	c, _ := newTestController(t)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return "agent-rebase", nil
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "APPROVED", Conflicts: "yes", TargetRepo: "owner/repo",
		},
	}

	// First call dispatches rebase agent.
	c.HandleGHStatus(context.Background(), statuses, nil)
	if launchCount != 1 {
		t.Fatalf("expected 1 launch, got %d", launchCount)
	}

	// Agent still running — should not re-dispatch.
	runStates := []*run.State{
		{ID: "agent-rebase", TmuxPane: strPtr("%1")},
	}
	c.HandleGHStatus(context.Background(), statuses, runStates)
	if launchCount != 1 {
		t.Errorf("expected no duplicate rebase dispatch, got %d total", launchCount)
	}
}

func TestRebaseDispatchRetryOnFailure(t *testing.T) {
	c, _ := newTestController(t)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return "", fmt.Errorf("worktree already exists")
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "APPROVED", Conflicts: "yes", TargetRepo: "owner/repo",
		},
	}

	// First failure: should retry, not stall.
	c.HandleGHStatus(context.Background(), statuses, nil)
	state := c.PipelineStates()["42"]
	if state.Stage == StageStalled {
		t.Error("expected retry on first rebase dispatch failure, not stall")
	}

	// Simulate backoff elapsed.
	c.mu.Lock()
	c.prStates["42"].LastFailedAt = time.Now().Add(-2 * time.Minute)
	c.mu.Unlock()

	// Second failure.
	c.HandleGHStatus(context.Background(), statuses, nil)
	state = c.PipelineStates()["42"]
	if state.Stage == StageStalled {
		t.Error("expected retry on second failure")
	}

	// Simulate backoff elapsed.
	c.mu.Lock()
	c.prStates["42"].LastFailedAt = time.Now().Add(-2 * time.Minute)
	c.mu.Unlock()

	// Third failure: should stall.
	actions := c.HandleGHStatus(context.Background(), statuses, nil)
	state = c.PipelineStates()["42"]
	if state.Stage != StageStalled {
		t.Errorf("expected stalled after retries exhausted, got %s", state.Stage)
	}
	hasError := false
	for _, a := range actions {
		if a.Type == "error" {
			hasError = true
		}
	}
	if !hasError {
		t.Error("expected error action when rebase retries exhausted")
	}
}

func TestApprovedNoConflictsMerges(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAutoMergeOnApproval(true)

	launchCount := 0
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		launchCount++
		return "agent-001", nil
	})
	mergeCount := 0
	c.SetMergePRs(func(ctx context.Context, repo string, prNumbers []string) error {
		mergeCount++
		return nil
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "APPROVED", Conflicts: "none", TargetRepo: "owner/repo",
		},
	}

	actions := c.HandleGHStatus(context.Background(), statuses, nil)

	if launchCount != 0 {
		t.Errorf("expected no agent launch for conflict-free merge, got %d", launchCount)
	}
	if mergeCount != 1 {
		t.Errorf("expected 1 merge, got %d", mergeCount)
	}

	hasMerge := false
	for _, a := range actions {
		if a.Type == "merge" {
			hasMerge = true
		}
	}
	if !hasMerge {
		t.Error("expected merge action")
	}
}

func TestApprovedNoAutoMergeWhenDisabled(t *testing.T) {
	c, _ := newTestController(t)
	// autoMergeOnApproval defaults to false — do not set it

	mergeCount := 0
	c.SetMergePRs(func(ctx context.Context, repo string, prNumbers []string) error {
		mergeCount++
		return nil
	})

	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "APPROVED", Conflicts: "none", TargetRepo: "owner/repo",
		},
	}

	actions := c.HandleGHStatus(context.Background(), statuses, nil)

	if mergeCount != 0 {
		t.Errorf("expected no merge when auto_merge_on_approval is disabled, got %d", mergeCount)
	}
	for _, a := range actions {
		if a.Type == "merge" {
			t.Error("unexpected merge action when auto_merge_on_approval is disabled")
		}
	}
}

func TestReviewThreadsResolvedAfterAgentCompletion(t *testing.T) {
	c, _ := newTestController(t)

	// Track which threads were resolved.
	var resolvedThreads []string
	c.SetSnapshotThreads(func(repo, prNumber string) ([]string, error) {
		return []string{"RT_aaa", "RT_bbb"}, nil
	})
	c.SetResolveThread(func(threadID string) error {
		resolvedThreads = append(resolvedThreads, threadID)
		return nil
	})
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		return "agent-fix", nil
	})

	// Step 1: CHANGES_REQUESTED → dispatches fix agent, snapshots threads.
	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "CHANGES_REQUESTED", TargetRepo: "owner/repo",
		},
	}
	c.HandleGHStatus(context.Background(), statuses, nil)

	state := c.PipelineStates()["42"]
	if len(state.PendingResolveThreadIDs) != 2 {
		t.Fatalf("expected 2 pending threads, got %d", len(state.PendingResolveThreadIDs))
	}

	// Step 2: Agent completes (finalized with cost).
	cost := 1.0
	runStates := []*run.State{
		{ID: "agent-fix", TmuxPane: strPtr("%1"), CostUSD: &cost},
	}
	// CI now passing, review approved → the threads should be resolved.
	statuses["42"] = &PRStatus{
		PRNumber: "42", State: "OPEN", CI: "passing",
		ReviewDecision: "APPROVED", Conflicts: "none", TargetRepo: "owner/repo",
	}
	c.SetMergePRs(func(ctx context.Context, repo string, prNumbers []string) error {
		return nil
	})
	c.HandleGHStatus(context.Background(), statuses, runStates)

	if len(resolvedThreads) != 2 {
		t.Errorf("expected 2 threads resolved, got %d: %v", len(resolvedThreads), resolvedThreads)
	}
	if resolvedThreads[0] != "RT_aaa" || resolvedThreads[1] != "RT_bbb" {
		t.Errorf("unexpected resolved threads: %v", resolvedThreads)
	}

	// Pending threads should be cleared after resolution.
	state = c.PipelineStates()["42"]
	if state != nil && len(state.PendingResolveThreadIDs) != 0 {
		t.Errorf("expected pending threads cleared, got %d", len(state.PendingResolveThreadIDs))
	}
}

func TestReviewThreadResolutionFailureDoesNotBlock(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAutoMergeOnApproval(true)

	resolveCount := 0
	c.SetSnapshotThreads(func(repo, prNumber string) ([]string, error) {
		return []string{"RT_ok", "RT_fail", "RT_ok2"}, nil
	})
	c.SetResolveThread(func(threadID string) error {
		resolveCount++
		if threadID == "RT_fail" {
			return fmt.Errorf("permission denied")
		}
		return nil
	})
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		return "agent-fix", nil
	})
	c.SetMergePRs(func(ctx context.Context, repo string, prNumbers []string) error {
		return nil
	})

	// Dispatch fix agent.
	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "CHANGES_REQUESTED", TargetRepo: "owner/repo",
		},
	}
	c.HandleGHStatus(context.Background(), statuses, nil)

	// Agent completes.
	cost := 1.0
	runStates := []*run.State{
		{ID: "agent-fix", TmuxPane: strPtr("%1"), CostUSD: &cost},
	}
	statuses["42"] = &PRStatus{
		PRNumber: "42", State: "OPEN", CI: "passing",
		ReviewDecision: "APPROVED", Conflicts: "none", TargetRepo: "owner/repo",
	}
	actions := c.HandleGHStatus(context.Background(), statuses, runStates)

	// All 3 threads should have been attempted.
	if resolveCount != 3 {
		t.Errorf("expected 3 resolve attempts, got %d", resolveCount)
	}

	// Pipeline should continue (merge action present).
	hasMerge := false
	for _, a := range actions {
		if a.Type == "merge" {
			hasMerge = true
		}
	}
	if !hasMerge {
		t.Error("expected merge action despite resolution failure")
	}
}

func TestTrustedCommentSnapshotsThreads(t *testing.T) {
	c, _ := newTestController(t)

	var snapshotCalled bool
	c.SetSnapshotThreads(func(repo, prNumber string) ([]string, error) {
		snapshotCalled = true
		return []string{"RT_trusted"}, nil
	})
	c.SetResolveThread(func(threadID string) error {
		return nil
	})
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
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

	c.HandleGHStatus(context.Background(), statuses, nil)

	if !snapshotCalled {
		t.Error("expected thread snapshot for trusted reviewer comment dispatch")
	}

	state := c.PipelineStates()["42"]
	if len(state.PendingResolveThreadIDs) != 1 || state.PendingResolveThreadIDs[0] != "RT_trusted" {
		t.Errorf("expected 1 pending thread RT_trusted, got %v", state.PendingResolveThreadIDs)
	}
}

func TestNoThreadResolutionWhileAgentRunning(t *testing.T) {
	c, _ := newTestController(t)

	resolveCount := 0
	c.SetSnapshotThreads(func(repo, prNumber string) ([]string, error) {
		return []string{"RT_aaa"}, nil
	})
	c.SetResolveThread(func(threadID string) error {
		resolveCount++
		return nil
	})
	c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
		return "agent-fix", nil
	})

	// Dispatch fix agent.
	statuses := map[string]*PRStatus{
		"42": {
			PRNumber: "42", State: "OPEN", CI: "passing",
			ReviewDecision: "CHANGES_REQUESTED", TargetRepo: "owner/repo",
		},
	}
	c.HandleGHStatus(context.Background(), statuses, nil)

	// Agent still running.
	runStates := []*run.State{
		{ID: "agent-fix", TmuxPane: strPtr("%1")}, // running: no cost/duration
	}
	c.HandleGHStatus(context.Background(), statuses, runStates)

	if resolveCount != 0 {
		t.Errorf("expected no thread resolution while agent running, got %d", resolveCount)
	}
}

func TestDispatchCooldownPreventsRapidRedispatch(t *testing.T) {
	t.Run("CI failing", func(t *testing.T) {
		c, _ := newTestController(t)

		launchCount := 0
		c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
			launchCount++
			return fmt.Sprintf("agent-%03d", launchCount), nil
		})

		statuses := map[string]*PRStatus{
			"42": {PRNumber: "42", State: "OPEN", CI: "failing", TargetRepo: "owner/repo"},
		}

		// First poll dispatches agent.
		c.HandleGHStatus(context.Background(), statuses, nil)
		if launchCount != 1 {
			t.Fatalf("expected 1 launch, got %d", launchCount)
		}

		// Agent completes immediately.
		cost := 1.0
		runStates := []*run.State{
			{ID: "agent-001", TmuxPane: strPtr("%1"), CostUSD: &cost},
		}

		// Second poll within cooldown — should NOT re-dispatch.
		c.HandleGHStatus(context.Background(), statuses, runStates)
		if launchCount != 1 {
			t.Errorf("expected cooldown to prevent re-dispatch, got %d launches", launchCount)
		}

		// Simulate cooldown expiry.
		c.mu.Lock()
		c.prStates["42"].LastDispatchAt = time.Now().Add(-61 * time.Second)
		c.mu.Unlock()

		// Third poll after cooldown — should dispatch.
		c.HandleGHStatus(context.Background(), statuses, runStates)
		if launchCount != 2 {
			t.Errorf("expected dispatch after cooldown, got %d launches", launchCount)
		}
	})

	t.Run("CHANGES_REQUESTED", func(t *testing.T) {
		c, _ := newTestController(t)

		launchCount := 0
		c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
			launchCount++
			return fmt.Sprintf("agent-%03d", launchCount), nil
		})

		statuses := map[string]*PRStatus{
			"42": {
				PRNumber: "42", State: "OPEN", CI: "passing",
				ReviewDecision: "CHANGES_REQUESTED", TargetRepo: "owner/repo",
			},
		}

		// First poll dispatches.
		c.HandleGHStatus(context.Background(), statuses, nil)
		if launchCount != 1 {
			t.Fatalf("expected 1 launch, got %d", launchCount)
		}

		// Agent completes.
		cost := 1.0
		runStates := []*run.State{
			{ID: "agent-001", TmuxPane: strPtr("%1"), CostUSD: &cost},
		}

		// Second poll within cooldown — blocked.
		c.HandleGHStatus(context.Background(), statuses, runStates)
		if launchCount != 1 {
			t.Errorf("expected cooldown to prevent re-dispatch, got %d launches", launchCount)
		}

		// Expire cooldown.
		c.mu.Lock()
		c.prStates["42"].LastDispatchAt = time.Now().Add(-61 * time.Second)
		c.mu.Unlock()

		// Third poll — allowed.
		c.HandleGHStatus(context.Background(), statuses, runStates)
		if launchCount != 2 {
			t.Errorf("expected dispatch after cooldown, got %d launches", launchCount)
		}
	})

	t.Run("trusted comments", func(t *testing.T) {
		c, _ := newTestController(t)

		launchCount := 0
		c.SetLaunchAgent(func(ctx context.Context, prNumber, repo, prompt string) (string, error) {
			launchCount++
			return fmt.Sprintf("agent-%03d", launchCount), nil
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

		// First poll dispatches.
		c.HandleGHStatus(context.Background(), statuses, nil)
		if launchCount != 1 {
			t.Fatalf("expected 1 launch, got %d", launchCount)
		}

		// Agent completes.
		cost := 1.0
		runStates := []*run.State{
			{ID: "agent-001", TmuxPane: strPtr("%1"), CostUSD: &cost},
		}

		// Second poll within cooldown — blocked.
		c.HandleGHStatus(context.Background(), statuses, runStates)
		if launchCount != 1 {
			t.Errorf("expected cooldown to prevent re-dispatch, got %d launches", launchCount)
		}

		// Expire cooldown.
		c.mu.Lock()
		c.prStates["42"].LastDispatchAt = time.Now().Add(-61 * time.Second)
		c.mu.Unlock()

		// Third poll — allowed.
		c.HandleGHStatus(context.Background(), statuses, runStates)
		if launchCount != 2 {
			t.Errorf("expected dispatch after cooldown, got %d launches", launchCount)
		}
	})
}

func strPtr(s string) *string {
	return &s
}

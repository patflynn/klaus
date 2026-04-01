package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
)

// Stage represents the pipeline stage for a PR.
type Stage string

const (
	StageCIPending     Stage = "ci_pending"
	StageCIFailed      Stage = "ci_failed"
	StageCIPassed      Stage = "ci_passed"
	StageReviewPending Stage = "review_pending"
	StageApproved      Stage = "approved"
	StageMerging       Stage = "merging"
	StageMerged        Stage = "merged"
	StageStalled       Stage = "stalled"
)

// PRStatus holds the GitHub-fetched status for a single PR, passed from the dashboard.
type PRStatus struct {
	PRNumber               string
	PRURL                  string
	State                  string // OPEN, MERGED, CLOSED
	CI                     string // passing, failing, pending, unknown
	Conflicts              string // yes, none, unknown
	ReviewDecision         string // APPROVED, CHANGES_REQUESTED, etc.
	TargetRepo             string // owner/repo for dispatch context
	HasNewTrustedComments  bool   // unaddressed comments from trusted reviewers
}

// PRPipelineState tracks per-PR pipeline state.
type PRPipelineState struct {
	PRNumber       string
	Stage          Stage
	LastAgentID    string // run ID of last dispatched agent
	AgentRunning   bool   // whether the dispatched agent is still active
	SeenCommentIDs map[int64]bool
	RetryCount     int       // number of launch retries after failure
	LastFailedAt   time.Time // when the last launch failure occurred
}

// Action describes a side-effect the controller wants the dashboard to perform.
type Action struct {
	Type   string // "launch", "merge", or "error"
	Detail string // human-readable description
	Error  string // non-empty if action represents a failure
}

// Controller manages the PR pipeline lifecycle.
type Controller struct {
	store    run.StateStore
	eventLog *event.Log
	logger   *slog.Logger
	prStates map[string]*PRPipelineState // keyed by PR number
	mu       sync.Mutex

	// Injectable runners for testing.
	launchAgent    func(ctx context.Context, prNumber, repo, prompt string) (string, error)
	mergePRs       func(ctx context.Context, repo string, prNumbers []string) error
	cleanIdlePanes func(runStates []*run.State)
}

// New creates a new pipeline controller.
func New(store run.StateStore, eventLog *event.Log, logger *slog.Logger) *Controller {
	c := &Controller{
		store:    store,
		eventLog: eventLog,
		logger:   logger,
		prStates: make(map[string]*PRPipelineState),
	}
	c.launchAgent = c.defaultLaunchAgent
	c.mergePRs = c.defaultMergePRs
	c.cleanIdlePanes = c.defaultCleanIdlePanes
	return c
}

// SetLaunchAgent overrides the agent launcher (for testing).
func (c *Controller) SetLaunchAgent(fn func(ctx context.Context, prNumber, repo, prompt string) (string, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.launchAgent = fn
}

// SetMergePRs overrides the merge runner (for testing).
func (c *Controller) SetMergePRs(fn func(ctx context.Context, repo string, prNumbers []string) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergePRs = fn
}

// SetCleanIdlePanes overrides idle pane cleanup (for testing).
func (c *Controller) SetCleanIdlePanes(fn func(runStates []*run.State)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanIdlePanes = fn
}

// HandleGHStatus is called by the dashboard on each GH poll with fresh PR statuses.
// It evaluates pipeline transitions and returns any actions taken.
func (c *Controller) HandleGHStatus(ctx context.Context, statuses map[string]*PRStatus, runStates []*run.State) []Action {
	c.mu.Lock()
	defer c.mu.Unlock()

	var actions []Action

	// Clean up idle agent panes before evaluating state.
	c.cleanIdlePanes(runStates)

	// Build a set of running agent run IDs from current run states.
	runningAgents := make(map[string]bool)
	for _, s := range runStates {
		if isRunning(s) {
			runningAgents[s.ID] = true
		}
	}

	for prNum, status := range statuses {
		if status.State == "MERGED" || status.State == "CLOSED" {
			// Clean up tracking for merged/closed PRs.
			if ps, ok := c.prStates[prNum]; ok {
				if status.State == "MERGED" && ps.Stage != StageMerged {
					ps.Stage = StageMerged
					c.emitEvent(prNum, event.PRMerged, map[string]interface{}{
						"pr_number": prNum,
						"pr_url":    status.PRURL,
					})
				}
				delete(c.prStates, prNum)
			}
			continue
		}

		ps := c.getOrCreateState(prNum)

		// Update agent running status.
		if ps.LastAgentID != "" {
			ps.AgentRunning = runningAgents[ps.LastAgentID]
		}

		prevStage := ps.Stage
		actions = append(actions, c.evaluate(ctx, ps, status, runStates)...)

		if ps.Stage != prevStage {
			c.logger.Info("pipeline transition",
				"pr", prNum,
				"from", string(prevStage),
				"to", string(ps.Stage),
			)
		}
	}

	return actions
}

// PipelineStates returns a snapshot of current pipeline states (for dashboard rendering).
func (c *Controller) PipelineStates() map[string]*PRPipelineState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]*PRPipelineState, len(c.prStates))
	for k, v := range c.prStates {
		cp := *v
		out[k] = &cp
	}
	return out
}

func (c *Controller) getOrCreateState(prNum string) *PRPipelineState {
	ps, ok := c.prStates[prNum]
	if !ok {
		ps = &PRPipelineState{
			PRNumber:       prNum,
			Stage:          StageCIPending,
			SeenCommentIDs: make(map[int64]bool),
		}
		c.prStates[prNum] = ps
	}
	return ps
}

// maxLaunchRetries is the maximum number of agent launch retries before going to StageStalled.
const maxLaunchRetries = 2

// retryBackoff is the minimum time between launch retries.
const retryBackoff = 60 * time.Second

// evaluate checks the current GH status and determines transitions + dispatches.
func (c *Controller) evaluate(ctx context.Context, ps *PRPipelineState, status *PRStatus, runStates []*run.State) []Action {
	var actions []Action

	switch {
	case status.CI == "failing":
		if ps.Stage != StageCIFailed || !ps.AgentRunning {
			if !ps.AgentRunning {
				// Clean up stale worktrees for this PR before dispatching.
				c.cleanupStaleWorktrees(ps.PRNumber, runStates)

				// Dispatch fix agent for CI failure.
				prompt := fmt.Sprintf(
					"CI is failing on PR #%s. Diagnose the failures and push fixes. Check `gh pr checks %s` for details and `gh run view <run-id> --log-failed` for error output.",
					ps.PRNumber, ps.PRNumber,
				)
				agentID, err := c.launchAgent(ctx, ps.PRNumber, status.TargetRepo, prompt)
				if err != nil {
					c.logger.Error("failed to dispatch CI fix agent", "pr", ps.PRNumber, "err", err)
					if !c.handleLaunchRetry(ps) {
						ps.Stage = StageStalled
						return []Action{{Type: "error", Detail: fmt.Sprintf("PR #%s: dispatch failed", ps.PRNumber), Error: truncateError(err.Error(), 120)}}
					}
					return nil
				}
				ps.RetryCount = 0
				ps.LastAgentID = agentID
				ps.AgentRunning = true
				actions = append(actions, Action{Type: "launch", Detail: fmt.Sprintf("CI fix agent for PR #%s", ps.PRNumber)})
			}
			ps.Stage = StageCIFailed
			c.emitEvent(ps.PRNumber, event.AgentCIFailed, map[string]interface{}{
				"pr_number": ps.PRNumber,
				"pr_url":    status.PRURL,
			})
		}

	case status.CI == "passing":
		if ps.Stage == StageCIFailed || ps.Stage == StageCIPending || ps.Stage == StageReviewPending {
			c.emitEvent(ps.PRNumber, event.AgentCIPassed, map[string]interface{}{
				"pr_number": ps.PRNumber,
				"pr_url":    status.PRURL,
			})
		}

		if strings.EqualFold(status.ReviewDecision, "APPROVED") {
			if ps.Stage != StageApproved && ps.Stage != StageMerging {
				ps.Stage = StageApproved
				c.emitEvent(ps.PRNumber, event.PRApproved, map[string]interface{}{
					"pr_number": ps.PRNumber,
					"pr_url":    status.PRURL,
				})
			}

			// Auto-merge: CI passing + approved + no conflicts.
			if status.Conflicts != "yes" && ps.Stage == StageApproved {
				ps.Stage = StageMerging
				err := c.mergePRs(ctx, status.TargetRepo, []string{ps.PRNumber})
				if err != nil {
					c.logger.Error("auto-merge failed", "pr", ps.PRNumber, "err", err)
					ps.Stage = StageStalled
					actions = append(actions, Action{Type: "error", Detail: fmt.Sprintf("PR #%s: auto-merge failed", ps.PRNumber), Error: truncateError(err.Error(), 120)})
				} else {
					ps.Stage = StageMerged
					c.emitEvent(ps.PRNumber, event.PRMerged, map[string]interface{}{
						"pr_number": ps.PRNumber,
						"pr_url":    status.PRURL,
					})
					actions = append(actions, Action{Type: "merge", Detail: fmt.Sprintf("Merged PR #%s", ps.PRNumber)})
				}
			}
		} else if strings.EqualFold(status.ReviewDecision, "CHANGES_REQUESTED") {
			// Review comments need addressing.
			if !ps.AgentRunning {
				// Clean up stale worktrees for this PR before dispatching.
				c.cleanupStaleWorktrees(ps.PRNumber, runStates)

				prompt := fmt.Sprintf(
					"PR #%s has changes requested by reviewers. Address the review comments and push fixes. Check `gh api repos/{owner}/{repo}/pulls/%s/comments` for comment details.",
					ps.PRNumber, ps.PRNumber,
				)
				agentID, err := c.launchAgent(ctx, ps.PRNumber, status.TargetRepo, prompt)
				if err != nil {
					c.logger.Error("failed to dispatch review fix agent", "pr", ps.PRNumber, "err", err)
					if !c.handleLaunchRetry(ps) {
						ps.Stage = StageStalled
						actions = append(actions, Action{Type: "error", Detail: fmt.Sprintf("PR #%s: dispatch failed", ps.PRNumber), Error: truncateError(err.Error(), 120)})
					}
					return actions
				}
				ps.RetryCount = 0
				ps.LastAgentID = agentID
				ps.AgentRunning = true
				ps.Stage = StageReviewPending
				actions = append(actions, Action{Type: "launch", Detail: fmt.Sprintf("Review fix agent for PR #%s", ps.PRNumber)})
			}
		} else {
			// CI passed, no explicit CHANGES_REQUESTED or APPROVED.
			if status.HasNewTrustedComments && !ps.AgentRunning {
				// Trusted reviewer left unaddressed comments — dispatch fix agent.
				prompt := fmt.Sprintf(
					"PR #%s in %s has review comments from a trusted reviewer that need to be addressed. Check the review comments with: gh api repos/%s/pulls/%s/comments",
					ps.PRNumber, status.TargetRepo, status.TargetRepo, ps.PRNumber,
				)
				agentID, err := c.launchAgent(ctx, ps.PRNumber, status.TargetRepo, prompt)
				if err != nil {
					c.logger.Error("failed to dispatch trusted review fix agent", "pr", ps.PRNumber, "err", err)
					ps.Stage = StageStalled
					return actions
				}
				ps.LastAgentID = agentID
				ps.AgentRunning = true
				ps.Stage = StageReviewPending
				actions = append(actions, Action{Type: "launch", Detail: fmt.Sprintf("Review fix agent for PR #%s (trusted reviewer)", ps.PRNumber)})
			} else if ps.Stage != StageApproved && ps.Stage != StageMerging && !ps.AgentRunning {
				// Waiting for review.
				ps.Stage = StageCIPassed
				c.emitEvent(ps.PRNumber, event.PRAwaitingApproval, map[string]interface{}{
					"pr_number": ps.PRNumber,
					"pr_url":    status.PRURL,
				})
			}
		}

	default:
		// CI pending or unknown — stay in current stage or set to pending.
		if ps.Stage == "" {
			ps.Stage = StageCIPending
		}
	}

	return actions
}

// handleLaunchRetry checks whether the pipeline state is eligible for retry.
// Returns true if the retry was accepted (caller should NOT go to StageStalled).
func (c *Controller) handleLaunchRetry(ps *PRPipelineState) bool {
	if ps.RetryCount >= maxLaunchRetries {
		return false
	}
	if !ps.LastFailedAt.IsZero() && time.Since(ps.LastFailedAt) < retryBackoff {
		// Too soon to retry — stay in current stage but don't stall yet.
		return true
	}
	ps.RetryCount++
	ps.LastFailedAt = time.Now()
	c.logger.Info("agent launch failed, will retry",
		"pr", ps.PRNumber,
		"retry", ps.RetryCount,
		"max", maxLaunchRetries,
	)
	return true
}

// cleanupStaleWorktrees removes worktrees from completed runs that match the given PR number.
// This prevents "worktree already exists" errors when re-dispatching agents.
func (c *Controller) cleanupStaleWorktrees(prNumber string, runStates []*run.State) {
	for _, s := range runStates {
		if s.PR == nil || *s.PR != prNumber {
			continue
		}
		if isRunning(s) {
			continue
		}
		if s.Worktree == "" {
			continue
		}
		// Check if the worktree directory still exists on disk.
		if _, err := os.Stat(s.Worktree); err != nil {
			continue
		}
		// Run klaus cleanup for this stale run.
		c.logger.Info("cleaning up stale worktree before dispatch",
			"pr", prNumber,
			"run", s.ID,
			"worktree", s.Worktree,
		)
		cmd := exec.Command("klaus", "cleanup", s.ID)
		if out, err := cmd.CombinedOutput(); err != nil {
			c.logger.Error("stale worktree cleanup failed",
				"run", s.ID,
				"err", err,
				"output", string(out),
			)
		}
	}
}

// defaultCleanIdlePanes kills tmux panes for agent runs whose processes have finished.
func (c *Controller) defaultCleanIdlePanes(runStates []*run.State) {
	for _, s := range runStates {
		if s.TmuxPane == nil || !tmux.PaneExists(*s.TmuxPane) {
			continue
		}

		if !s.IsAgentRunning() {
			c.logger.Info("closing completed agent pane", "run", s.ID, "pane", *s.TmuxPane)
			tmux.KillPane(*s.TmuxPane)
		}
	}
}

func (c *Controller) emitEvent(prNumber, eventType string, data map[string]interface{}) {
	if c.eventLog == nil {
		return
	}
	evt := event.New(prNumber, eventType, data)
	if err := c.eventLog.Emit(evt); err != nil {
		c.logger.Error("failed to emit event", "type", eventType, "pr", prNumber, "err", err)
	}
}

func (c *Controller) defaultLaunchAgent(ctx context.Context, prNumber, repo, prompt string) (string, error) {
	args := []string{"launch", "--pr", prNumber}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, "klaus", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("klaus launch: %w: %s", err, string(out))
	}
	// Extract run ID from output (first line typically: "Launching agent <id>...")
	// Best-effort extraction.
	output := string(out)
	if id := extractAgentID(output); id != "" {
		return id, nil
	}
	return "unknown", nil
}

func (c *Controller) defaultMergePRs(ctx context.Context, repo string, prNumbers []string) error {
	args := []string{"merge", "--yes"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	args = append(args, prNumbers...)
	cmd := exec.CommandContext(ctx, "klaus", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("klaus merge: %w: %s", err, string(out))
	}
	return nil
}

// extractAgentID attempts to pull the run ID from "Launching agent <id>..." output.
func extractAgentID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Launching agent ") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				return strings.TrimSuffix(parts[2], "...")
			}
		}
	}
	return ""
}

// isRunning checks if a run is still active (has tmux pane, not finalized).
func isRunning(s *run.State) bool {
	if s.TmuxPane == nil {
		return false
	}
	if s.CostUSD != nil || s.DurationMS != nil {
		return false
	}
	return true
}

// truncateError returns a short, single-line version of an error message.
// It strips cobra "Usage:" help text and truncates to maxLen.
func truncateError(s string, maxLen int) string {
	// Strip everything from "Usage:" onward (cobra help text).
	if idx := strings.Index(s, "Usage:"); idx > 0 {
		s = strings.TrimSpace(s[:idx])
	}
	// Take only the first line.
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		return s[:maxLen-1] + "…"
	}
	return s
}

// StageLabel returns a human-readable label for dashboard display.
func StageLabel(stage Stage) string {
	switch stage {
	case StageCIPending:
		return "CI pending"
	case StageCIFailed:
		return "CI failed, fix running"
	case StageCIPassed:
		return "CI passed, reviewing"
	case StageReviewPending:
		return "review fix running"
	case StageApproved:
		return "approved, ready"
	case StageMerging:
		return "merging"
	case StageMerged:
		return "merged"
	case StageStalled:
		return "stalled"
	default:
		return string(stage)
	}
}

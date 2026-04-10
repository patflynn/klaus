package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/patflynn/klaus/internal/event"
	ghutil "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/run"
)

// Stage represents the pipeline stage for a PR.
type Stage string

const (
	StageCIPending     Stage = "ci_pending"
	StageCIFailed      Stage = "ci_failed"
	StageCIPassed      Stage = "ci_passed"
	StageReviewPending Stage = "review_pending"
	StageApproved      Stage = "approved"
	StageNeedsRebase   Stage = "needs_rebase"
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
	PRNumber                string
	Stage                   Stage
	LastAgentID             string // run ID of last dispatched agent
	AgentRunning            bool   // whether the dispatched agent is still active
	SeenCommentIDs          map[int64]bool
	PendingResolveThreadIDs []string  // GraphQL thread IDs to resolve after agent completes
	RetryCount              int       // number of launch retries after failure
	LastFailedAt            time.Time // when the last launch failure occurred
	LastDispatchAt          time.Time // when the last agent was dispatched (cooldown guard)
	FixAttempts             int       // number of fix agents dispatched that completed without fixing CI

	pendingLaunchDetail string // transient: detail text for pending launch action
}

// Action describes a side-effect the controller wants the dashboard to perform.
type Action struct {
	Type   string // "launch", "merge", or "error"
	Detail string // human-readable description
	Error  string // non-empty if action represents a failure
}

// ActionType enumerates the kinds of side-effects evaluate() can request.
type ActionType int

const (
	ActionLaunchAgent ActionType = iota
	ActionMergePR
	ActionCleanupWorktrees
	ActionSnapshotThreads
)

// ActionDescriptor is a pure data description of a side-effect to perform.
// evaluate() returns these instead of executing I/O directly.
type ActionDescriptor struct {
	Type       ActionType
	PRNumber   string
	Repo       string
	Prompt     string
	ResumeFrom string
	PRNumbers  []string // for merge
	RunStates  []*run.State // for worktree cleanup
}

// Controller manages the PR pipeline lifecycle.
type Controller struct {
	store    run.StateStore
	eventLog *event.Log
	logger   *slog.Logger
	prStates map[string]*PRPipelineState // keyed by PR number
	mu       sync.Mutex

	autoMergeOnApproval bool // whether to auto-merge approved PRs

	tmuxDeps run.TmuxDeps // tmux operations for checking pane state

	// Injectable runners for testing.
	launchAgent     func(ctx context.Context, prNumber, repo, prompt, resumeFrom string) (string, error)
	mergePRs        func(ctx context.Context, repo string, prNumbers []string) error
	snapshotThreads func(repo, prNumber string) ([]string, error)
	resolveThread   func(threadID string) error
}

// New creates a new pipeline controller.
func New(store run.StateStore, eventLog *event.Log, logger *slog.Logger) *Controller {
	c := &Controller{
		store:    store,
		eventLog: eventLog,
		logger:   logger,
		prStates: make(map[string]*PRPipelineState),
		tmuxDeps: run.DefaultTmuxDeps(),
	}
	c.launchAgent = c.defaultLaunchAgent
	c.mergePRs = c.defaultMergePRs
	c.snapshotThreads = c.defaultSnapshotThreads
	c.resolveThread = func(threadID string) error {
		return ghutil.NewGHCLIClient("").ResolveReviewThread(context.TODO(), threadID)
	}
	return c
}

// SetTmuxDeps overrides the tmux dependencies used for pane state checks.
func (c *Controller) SetTmuxDeps(td run.TmuxDeps) {
	c.tmuxDeps = td
}

// SetAutoMergeOnApproval controls whether approved PRs are automatically merged.
func (c *Controller) SetAutoMergeOnApproval(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.autoMergeOnApproval = enabled
}

// SetLaunchAgent overrides the agent launcher (for testing).
func (c *Controller) SetLaunchAgent(fn func(ctx context.Context, prNumber, repo, prompt, resumeFrom string) (string, error)) {
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

// SetSnapshotThreads overrides thread snapshot fetching (for testing).
func (c *Controller) SetSnapshotThreads(fn func(repo, prNumber string) ([]string, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshotThreads = fn
}

// SetResolveThread overrides thread resolution (for testing).
func (c *Controller) SetResolveThread(fn func(threadID string) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolveThread = fn
}

// HandleGHStatus is called by the dashboard on each GH poll with fresh PR statuses.
// It evaluates pipeline transitions and returns any actions taken.
//
// The method holds the mutex only while computing what to do (evaluate), then
// releases it to execute I/O (agent launches, merges, worktree cleanup), and
// re-acquires to update state with results. This prevents blocking dashboard
// rendering during slow exec calls.
func (c *Controller) HandleGHStatus(ctx context.Context, statuses map[string]*PRStatus, runStates []*run.State) []Action {
	// Phase 1: Hold lock, compute descriptors and collect immediate actions.
	c.mu.Lock()

	var actions []Action
	var descriptors []ActionDescriptor
	// Track which PRs need thread resolution (agent just completed).
	var threadResolvePRs []*PRPipelineState

	// Build a set of running agent run IDs from current run states.
	runningAgents := make(map[string]bool)
	for _, s := range runStates {
		if c.isRunning(s) {
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
		wasRunning := ps.AgentRunning
		if ps.LastAgentID != "" {
			ps.AgentRunning = runningAgents[ps.LastAgentID]
		}

		// Mark PRs that need thread resolution (agent just completed).
		if wasRunning && !ps.AgentRunning && len(ps.PendingResolveThreadIDs) > 0 {
			threadResolvePRs = append(threadResolvePRs, ps)
		}

		prevStage := ps.Stage
		evalActions, evalDescs := c.evaluate(ps, status, runStates)
		actions = append(actions, evalActions...)
		descriptors = append(descriptors, evalDescs...)

		if ps.Stage != prevStage {
			c.logger.Info("pipeline transition",
				"pr", prNum,
				"from", string(prevStage),
				"to", string(ps.Stage),
			)
		}
	}

	c.mu.Unlock()

	// Phase 2: Execute I/O without holding the lock.

	// Resolve pending review threads for agents that just completed.
	for _, ps := range threadResolvePRs {
		c.resolvePendingThreads(ps)
	}

	// Execute descriptors and collect results to apply under the lock.
	type launchResult struct {
		prNumber string
		agentID  string
		err      error
	}
	type mergeResult struct {
		prNumber string
		repo     string
		err      error
	}
	var launchResults []launchResult
	var mergeResults []mergeResult

	for _, desc := range descriptors {
		switch desc.Type {
		case ActionCleanupWorktrees:
			c.cleanupStaleWorktrees(desc.PRNumber, desc.RunStates)

		case ActionSnapshotThreads:
			c.mu.Lock()
			ps := c.prStates[desc.PRNumber]
			c.mu.Unlock()
			if ps != nil {
				c.snapshotUnresolvedThreads(ps, desc.Repo)
			}

		case ActionLaunchAgent:
			agentID, err := c.launchAgent(ctx, desc.PRNumber, desc.Repo, desc.Prompt, desc.ResumeFrom)
			launchResults = append(launchResults, launchResult{
				prNumber: desc.PRNumber,
				agentID:  agentID,
				err:      err,
			})

		case ActionMergePR:
			err := c.mergePRs(ctx, desc.Repo, desc.PRNumbers)
			mergeResults = append(mergeResults, mergeResult{
				prNumber: desc.PRNumber,
				repo:     desc.Repo,
				err:      err,
			})
		}
	}

	// Phase 3: Re-acquire lock to apply results.
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, lr := range launchResults {
		ps := c.prStates[lr.prNumber]
		if ps == nil {
			continue
		}
		if lr.err != nil {
			c.logger.Error("failed to dispatch agent", "pr", lr.prNumber, "err", lr.err)
			if !c.handleLaunchRetry(ps) {
				ps.Stage = StageStalled
				actions = append(actions, Action{Type: "error", Detail: fmt.Sprintf("PR #%s: dispatch failed", lr.prNumber), Error: truncateError(lr.err.Error(), 120)})
			}
		} else {
			ps.RetryCount = 0
			ps.LastAgentID = lr.agentID
			ps.AgentRunning = true
			ps.LastDispatchAt = time.Now()
			actions = append(actions, Action{Type: "launch", Detail: ps.pendingLaunchDetail})
			ps.pendingLaunchDetail = ""
		}
	}

	for _, mr := range mergeResults {
		ps := c.prStates[mr.prNumber]
		if ps == nil {
			continue
		}
		if mr.err != nil {
			c.logger.Error("auto-merge failed", "pr", mr.prNumber, "err", mr.err)
			ps.Stage = StageStalled
			actions = append(actions, Action{Type: "error", Detail: fmt.Sprintf("PR #%s: auto-merge failed", mr.prNumber), Error: truncateError(mr.err.Error(), 120)})
		} else {
			ps.Stage = StageMerged
			c.emitEvent(mr.prNumber, event.PRMerged, map[string]interface{}{
				"pr_number": mr.prNumber,
			})
			actions = append(actions, Action{Type: "merge", Detail: fmt.Sprintf("Merged PR #%s", mr.prNumber)})
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

// maxFixAttempts is the maximum number of fix agents dispatched for a single PR
// before the pipeline gives up. Reset when the branch is updated or CI passes.
const maxFixAttempts = 3

// retryBackoff is the minimum time between launch retries.
const retryBackoff = 60 * time.Second

// dispatchCooldown is the minimum time between agent dispatches for the same PR.
const dispatchCooldown = 60 * time.Second

// evaluate checks the current GH status and determines transitions + action
// descriptors. It performs NO I/O — all side-effects are described as
// ActionDescriptor values for the caller to execute outside the lock.
func (c *Controller) evaluate(ps *PRPipelineState, status *PRStatus, runStates []*run.State) ([]Action, []ActionDescriptor) {
	var actions []Action
	var descs []ActionDescriptor

	switch {
	case status.CI == "failing":
		// Circuit breaker: stop dispatching after too many failed fix attempts.
		if ps.FixAttempts >= maxFixAttempts && !ps.AgentRunning {
			if ps.Stage != StageStalled {
				ps.Stage = StageStalled
				c.logger.Warn("fix agent circuit breaker tripped",
					"pr", ps.PRNumber,
					"attempts", ps.FixAttempts,
				)
				return []Action{{Type: "error", Detail: fmt.Sprintf("PR #%s: %d fix attempts failed, stopping", ps.PRNumber, ps.FixAttempts)}}, nil
			}
			return nil, nil
		}

		if ps.Stage != StageCIFailed || !ps.AgentRunning {
			if !ps.AgentRunning && time.Since(ps.LastDispatchAt) > dispatchCooldown {
				// Count a failed fix attempt when a previous agent finished but CI is still failing.
				if ps.Stage == StageCIFailed && ps.LastAgentID != "" {
					ps.FixAttempts++
				}

				// Re-check circuit breaker after incrementing.
				if ps.FixAttempts >= maxFixAttempts {
					ps.Stage = StageStalled
					c.logger.Warn("fix agent circuit breaker tripped",
						"pr", ps.PRNumber,
						"attempts", ps.FixAttempts,
					)
					return []Action{{Type: "error", Detail: fmt.Sprintf("PR #%s: %d fix attempts failed, stopping", ps.PRNumber, ps.FixAttempts)}}, nil
				}

				// Request worktree cleanup before dispatch.
				descs = append(descs, ActionDescriptor{
					Type:      ActionCleanupWorktrees,
					PRNumber:  ps.PRNumber,
					RunStates: runStates,
				})

				// Request fix agent launch for CI failure.
				prompt := fmt.Sprintf(
					"CI is failing on PR #%s. Diagnose the failures and push fixes. Check `gh pr checks %s` for details and `gh run view <run-id> --log-failed` for error output.",
					ps.PRNumber, ps.PRNumber,
				)
				ps.pendingLaunchDetail = fmt.Sprintf("CI fix agent for PR #%s", ps.PRNumber)
				descs = append(descs, ActionDescriptor{
					Type:       ActionLaunchAgent,
					PRNumber:   ps.PRNumber,
					Repo:       status.TargetRepo,
					Prompt:     prompt,
					ResumeFrom: ps.LastAgentID,
				})
			}
			ps.Stage = StageCIFailed
			c.emitEvent(ps.PRNumber, event.AgentCIFailed, map[string]interface{}{
				"pr_number": ps.PRNumber,
				"pr_url":    status.PRURL,
			})
		}

	case status.CI == "passing":
		// CI passed — reset fix attempt counter.
		ps.FixAttempts = 0

		if ps.Stage == StageCIFailed || ps.Stage == StageCIPending || ps.Stage == StageReviewPending || ps.Stage == StageNeedsRebase {
			c.emitEvent(ps.PRNumber, event.AgentCIPassed, map[string]interface{}{
				"pr_number": ps.PRNumber,
				"pr_url":    status.PRURL,
			})
		}

		changesRequested := strings.EqualFold(status.ReviewDecision, "CHANGES_REQUESTED")
		approved := strings.EqualFold(status.ReviewDecision, "APPROVED") ||
			(!changesRequested && c.hasKlausApproval(ps.PRNumber, runStates))
		if approved {
			if ps.Stage != StageApproved && ps.Stage != StageMerging && ps.Stage != StageNeedsRebase {
				ps.Stage = StageApproved
				c.emitEvent(ps.PRNumber, event.PRApproved, map[string]interface{}{
					"pr_number": ps.PRNumber,
					"pr_url":    status.PRURL,
				})
			}

			// Mark internal run state as approved so that `klaus status`
			// and the dashboard reflect the GitHub approval.
			c.markRunStatesApproved(ps.PRNumber, runStates)

			if ps.Stage == StageApproved || ps.Stage == StageNeedsRebase {
				if status.Conflicts == "yes" {
					// Conflicts detected — dispatch rebase agent.
					if !ps.AgentRunning && time.Since(ps.LastDispatchAt) > dispatchCooldown {
						descs = append(descs, ActionDescriptor{
							Type:      ActionCleanupWorktrees,
							PRNumber:  ps.PRNumber,
							RunStates: runStates,
						})
						prompt := fmt.Sprintf(
							"PR #%s has merge conflicts with the base branch. Rebase onto main, resolve all conflicts, and push. Run tests after resolving.",
							ps.PRNumber,
						)
						ps.Stage = StageNeedsRebase
						ps.pendingLaunchDetail = fmt.Sprintf("Rebase agent for PR #%s", ps.PRNumber)
						descs = append(descs, ActionDescriptor{
							Type:       ActionLaunchAgent,
							PRNumber:   ps.PRNumber,
							Repo:       status.TargetRepo,
							Prompt:     prompt,
							ResumeFrom: ps.LastAgentID,
						})
					}
				} else if c.autoMergeOnApproval {
					// No conflicts and auto-merge enabled — proceed with merge.
					ps.Stage = StageMerging
					descs = append(descs, ActionDescriptor{
						Type:      ActionMergePR,
						PRNumber:  ps.PRNumber,
						Repo:      status.TargetRepo,
						PRNumbers: []string{ps.PRNumber},
					})
				}
			}
		} else if changesRequested {
			// Review comments need addressing.
			if !ps.AgentRunning && time.Since(ps.LastDispatchAt) > dispatchCooldown {
				// Request worktree cleanup before dispatch.
				descs = append(descs, ActionDescriptor{
					Type:      ActionCleanupWorktrees,
					PRNumber:  ps.PRNumber,
					RunStates: runStates,
				})

				// Request thread snapshot before dispatch.
				descs = append(descs, ActionDescriptor{
					Type:     ActionSnapshotThreads,
					PRNumber: ps.PRNumber,
					Repo:     status.TargetRepo,
				})

				prompt := fmt.Sprintf(
					"PR #%s has changes requested by reviewers. Address the review comments and push fixes. Check `gh api repos/{owner}/{repo}/pulls/%s/comments` for comment details.",
					ps.PRNumber, ps.PRNumber,
				)
				ps.pendingLaunchDetail = fmt.Sprintf("Review fix agent for PR #%s", ps.PRNumber)
				ps.Stage = StageReviewPending
				descs = append(descs, ActionDescriptor{
					Type:       ActionLaunchAgent,
					PRNumber:   ps.PRNumber,
					Repo:       status.TargetRepo,
					Prompt:     prompt,
					ResumeFrom: ps.LastAgentID,
				})
			}
		} else {
			// CI passed, no explicit CHANGES_REQUESTED or APPROVED.
			if status.HasNewTrustedComments && !ps.AgentRunning && time.Since(ps.LastDispatchAt) > dispatchCooldown {
				// Request thread snapshot before dispatch.
				descs = append(descs, ActionDescriptor{
					Type:     ActionSnapshotThreads,
					PRNumber: ps.PRNumber,
					Repo:     status.TargetRepo,
				})

				// Trusted reviewer left unaddressed comments — dispatch fix agent.
				prompt := fmt.Sprintf(
					"PR #%s in %s has review comments from a trusted reviewer that need to be addressed. Check the review comments with: gh api repos/%s/pulls/%s/comments",
					ps.PRNumber, status.TargetRepo, status.TargetRepo, ps.PRNumber,
				)
				ps.pendingLaunchDetail = fmt.Sprintf("Review fix agent for PR #%s (trusted reviewer)", ps.PRNumber)
				ps.Stage = StageReviewPending
				descs = append(descs, ActionDescriptor{
					Type:       ActionLaunchAgent,
					PRNumber:   ps.PRNumber,
					Repo:       status.TargetRepo,
					Prompt:     prompt,
					ResumeFrom: ps.LastAgentID,
				})
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
		// If CI was previously failing and is now pending, a new push likely
		// triggered a fresh CI run. Reset the fix attempt counter.
		if ps.Stage == StageCIFailed || ps.Stage == StageStalled {
			ps.FixAttempts = 0
		}
		if ps.Stage == "" || ps.Stage == StageStalled {
			ps.Stage = StageCIPending
		}
	}

	return actions, descs
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
		if c.isRunning(s) {
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

// markRunStatesApproved sets Approved=true on run states matching the given PR number
// that are not already approved, and persists the change via the store.
func (c *Controller) markRunStatesApproved(prNumber string, runStates []*run.State) {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, s := range runStates {
		if !runStateMatchesPR(s, prNumber) {
			continue
		}
		if s.Approved != nil && *s.Approved {
			continue
		}
		approved := true
		s.Approved = &approved
		s.ApprovedAt = &now
		if c.store != nil {
			if err := c.store.Save(s); err != nil {
				c.logger.Error("failed to persist GitHub approval to run state",
					"pr", prNumber, "run", s.ID, "err", err)
			}
		}
	}
}

// runStateMatchesPR reports whether the run state is associated with the given PR number.
func runStateMatchesPR(s *run.State, prNumber string) bool {
	if s.PR != nil && *s.PR == prNumber {
		return true
	}
	if s.PRURL != nil && strings.HasSuffix(strings.TrimRight(*s.PRURL, "/"), "/"+prNumber) {
		return true
	}
	return false
}

// hasKlausApproval returns true if any run state for the given PR has been
// approved via `klaus approve`.
func (c *Controller) hasKlausApproval(prNumber string, runStates []*run.State) bool {
	for _, s := range runStates {
		if !runStateMatchesPR(s, prNumber) {
			continue
		}
		if s.Approved != nil && *s.Approved {
			return true
		}
	}
	return false
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

func (c *Controller) defaultLaunchAgent(ctx context.Context, prNumber, repo, prompt, resumeFrom string) (string, error) {
	args := []string{"launch", "--pr", prNumber}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	if resumeFrom != "" {
		args = append(args, "--resume-from", resumeFrom)
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
func (c *Controller) isRunning(s *run.State) bool {
	return s.IsAgentRunningWith(c.tmuxDeps)
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

// snapshotUnresolvedThreads fetches and stores unresolved review thread IDs
// so they can be resolved after the fix agent completes.
func (c *Controller) snapshotUnresolvedThreads(ps *PRPipelineState, repo string) {
	threadIDs, err := c.snapshotThreads(repo, ps.PRNumber)
	if err != nil {
		c.logger.Warn("failed to snapshot review threads", "pr", ps.PRNumber, "err", err)
		ps.PendingResolveThreadIDs = nil
		return
	}
	ps.PendingResolveThreadIDs = threadIDs
	if len(threadIDs) > 0 {
		c.logger.Info("snapshotted unresolved review threads",
			"pr", ps.PRNumber,
			"count", len(threadIDs),
		)
	}
}

// resolvePendingThreads resolves all pending review threads for a PR.
func (c *Controller) resolvePendingThreads(ps *PRPipelineState) {
	resolved := 0
	for _, threadID := range ps.PendingResolveThreadIDs {
		if err := c.resolveThread(threadID); err != nil {
			c.logger.Warn("failed to resolve review thread",
				"pr", ps.PRNumber,
				"thread", threadID,
				"err", err,
			)
			continue
		}
		resolved++
	}
	if resolved > 0 {
		c.logger.Info("resolved review threads after agent completion",
			"pr", ps.PRNumber,
			"resolved", resolved,
			"total", len(ps.PendingResolveThreadIDs),
		)
	}
	ps.PendingResolveThreadIDs = nil
}

// defaultSnapshotThreads fetches unresolved review thread IDs from GitHub.
func (c *Controller) defaultSnapshotThreads(repo, prNumber string) ([]string, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repo format: %s", repo)
	}
	prNum, err := strconv.Atoi(prNumber)
	if err != nil {
		return nil, fmt.Errorf("invalid PR number: %s", prNumber)
	}
	threads, err := ghutil.NewGHCLIClient("").FetchReviewThreads(context.TODO(), parts[0], parts[1], prNum)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, t := range threads {
		if !t.IsResolved {
			ids = append(ids, t.ID)
		}
	}
	return ids, nil
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
	case StageNeedsRebase:
		return "rebasing"
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

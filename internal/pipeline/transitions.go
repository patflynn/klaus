package pipeline

import (
	"fmt"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
)

// transition defines a single state-machine rule. Guards are evaluated
// top-to-bottom; the first matching transition fires. An Apply function
// mutates pipeline state and returns any actions/descriptors.
type transition struct {
	Name  string
	Guard func(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) bool
	Apply func(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) ([]Action, []ActionDescriptor)
}

// transitions is the ordered list of pipeline rules. First match wins.
var transitions = []transition{
	// ── CI failing ──────────────────────────────────────────────────────

	{
		Name: "ci-failing/circuit-breaker-already-stalled",
		Guard: allOf(
			ciFailing,
			fixAttemptsExhausted,
			agentNotRunning,
			inStage(StageStalled),
		),
		Apply: func(_ *Controller, _ *PRPipelineState, _ *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			return nil, nil
		},
	},
	{
		Name: "ci-failing/circuit-breaker-trip",
		Guard: allOf(
			ciFailing,
			fixAttemptsExhausted,
			agentNotRunning,
		),
		Apply: func(c *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			ps.Stage = StageStalled
			c.logger.Warn("fix agent circuit breaker tripped",
				"pr", ps.PRNumber,
				"attempts", ps.FixAttempts,
			)
			return []Action{{
				Type:   "error",
				Detail: fmt.Sprintf("PR #%s: %d fix attempts failed, stopping", ps.PRNumber, ps.FixAttempts),
			}}, nil
		},
	},
	{
		Name: "ci-failing/dispatch-fix-agent",
		Guard: allOf(
			ciFailing,
			notInStageWhileRunning(StageCIFailed),
			agentNotRunning,
			cooldownExpired,
		),
		Apply: func(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) ([]Action, []ActionDescriptor) {
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
				return []Action{{
					Type:   "error",
					Detail: fmt.Sprintf("PR #%s: %d fix attempts failed, stopping", ps.PRNumber, ps.FixAttempts),
				}}, nil
			}

			var descs []ActionDescriptor
			descs = append(descs, ActionDescriptor{
				Type:      ActionCleanupWorktrees,
				PRNumber:  ps.PRNumber,
				RunStates: runStates,
			})

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

			ps.Stage = StageCIFailed
			c.emitEvent(ps.PRNumber, event.AgentCIFailed, map[string]interface{}{
				"pr_number": ps.PRNumber,
				"pr_url":    status.PRURL,
			})
			return nil, descs
		},
	},
	{
		Name: "ci-failing/update-stage",
		Guard: allOf(
			ciFailing,
			notInStageWhileRunning(StageCIFailed),
		),
		Apply: func(c *Controller, ps *PRPipelineState, status *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			if ps.Stage != StageCIFailed {
				c.emitEvent(ps.PRNumber, event.AgentCIFailed, map[string]interface{}{
					"pr_number": ps.PRNumber,
					"pr_url":    status.PRURL,
				})
			}
			ps.Stage = StageCIFailed
			return nil, nil
		},
	},
	{
		Name: "ci-failing/noop-already-handling",
		Guard: ciFailing,
		Apply: func(_ *Controller, _ *PRPipelineState, _ *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			return nil, nil
		},
	},

	// ── CI passing + approved + conflicts → rebase ──────────────────────

	{
		Name: "ci-passing/approved-conflicts-dispatch-rebase",
		Guard: allOf(
			ciPassing,
			isApproved,
			hasConflicts,
			agentNotRunning,
			cooldownExpired,
		),
		Apply: func(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) ([]Action, []ActionDescriptor) {
			ps.FixAttempts = 0
			emitCIPassedIfNeeded(c, ps, status)
			setApprovedIfNeeded(c, ps, status)
			c.markRunStatesApproved(ps.PRNumber, runStates)

			var descs []ActionDescriptor
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
			return nil, descs
		},
	},
	{
		Name: "ci-passing/approved-conflicts-wait",
		Guard: allOf(
			ciPassing,
			isApproved,
			hasConflicts,
		),
		Apply: func(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) ([]Action, []ActionDescriptor) {
			ps.FixAttempts = 0
			emitCIPassedIfNeeded(c, ps, status)
			setApprovedIfNeeded(c, ps, status)
			c.markRunStatesApproved(ps.PRNumber, runStates)
			return nil, nil
		},
	},

	// ── CI passing + approved + no conflicts → merge ────────────────────

	{
		Name: "ci-passing/approved-auto-merge",
		Guard: allOf(
			ciPassing,
			isApproved,
			noConflicts,
			autoMergeEnabled,
		),
		Apply: func(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) ([]Action, []ActionDescriptor) {
			ps.FixAttempts = 0
			emitCIPassedIfNeeded(c, ps, status)
			setApprovedIfNeeded(c, ps, status)
			c.markRunStatesApproved(ps.PRNumber, runStates)

			ps.Stage = StageMerging
			return nil, []ActionDescriptor{{
				Type:      ActionMergePR,
				PRNumber:  ps.PRNumber,
				Repo:      status.TargetRepo,
				PRNumbers: []string{ps.PRNumber},
			}}
		},
	},
	{
		Name: "ci-passing/approved-no-auto-merge",
		Guard: allOf(
			ciPassing,
			isApproved,
		),
		Apply: func(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) ([]Action, []ActionDescriptor) {
			ps.FixAttempts = 0
			emitCIPassedIfNeeded(c, ps, status)
			setApprovedIfNeeded(c, ps, status)
			c.markRunStatesApproved(ps.PRNumber, runStates)
			return nil, nil
		},
	},

	// ── CI passing + changes requested ──────────────────────────────────

	{
		Name: "ci-passing/changes-requested-dispatch",
		Guard: allOf(
			ciPassing,
			changesRequested,
			agentNotRunning,
			cooldownExpired,
		),
		Apply: func(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) ([]Action, []ActionDescriptor) {
			ps.FixAttempts = 0
			emitCIPassedIfNeeded(c, ps, status)

			var descs []ActionDescriptor
			descs = append(descs, ActionDescriptor{
				Type:      ActionCleanupWorktrees,
				PRNumber:  ps.PRNumber,
				RunStates: runStates,
			})
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
			return nil, descs
		},
	},
	{
		Name: "ci-passing/changes-requested-wait",
		Guard: allOf(
			ciPassing,
			changesRequested,
		),
		Apply: func(c *Controller, ps *PRPipelineState, status *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			ps.FixAttempts = 0
			emitCIPassedIfNeeded(c, ps, status)
			return nil, nil
		},
	},

	// ── CI passing + trusted comments ───────────────────────────────────

	{
		Name: "ci-passing/trusted-comments-dispatch",
		Guard: allOf(
			ciPassing,
			hasTrustedComments,
			agentNotRunning,
			cooldownExpired,
		),
		Apply: func(c *Controller, ps *PRPipelineState, status *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			ps.FixAttempts = 0
			emitCIPassedIfNeeded(c, ps, status)

			var descs []ActionDescriptor
			descs = append(descs, ActionDescriptor{
				Type:     ActionSnapshotThreads,
				PRNumber: ps.PRNumber,
				Repo:     status.TargetRepo,
			})

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
			return nil, descs
		},
	},

	// ── CI passing + awaiting review ────────────────────────────────────

	{
		Name: "ci-passing/awaiting-review",
		Guard: allOf(
			ciPassing,
			notInStages(StageApproved, StageMerging),
			agentNotRunning,
		),
		Apply: func(c *Controller, ps *PRPipelineState, status *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			ps.FixAttempts = 0
			emitCIPassedIfNeeded(c, ps, status)
			if ps.Stage != StageCIPassed {
				c.emitEvent(ps.PRNumber, event.PRAwaitingApproval, map[string]interface{}{
					"pr_number": ps.PRNumber,
					"pr_url":    status.PRURL,
				})
			}
			ps.Stage = StageCIPassed
			return nil, nil
		},
	},

	// CI passing catch-all (agent running, or already approved/merging with no action needed).
	{
		Name: "ci-passing/noop",
		Guard: ciPassing,
		Apply: func(_ *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			ps.FixAttempts = 0
			return nil, nil
		},
	},

	// ── CI pending / unknown ────────────────────────────────────────────

	{
		Name: "ci-pending/reset-from-failed",
		Guard: allOf(
			ciPendingOrUnknown,
			inStages(StageCIFailed, StageStalled),
		),
		Apply: func(_ *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			ps.FixAttempts = 0
			if ps.Stage == StageStalled {
				ps.Stage = StageCIPending
			}
			return nil, nil
		},
	},
	{
		Name: "ci-pending/init",
		Guard: allOf(
			ciPendingOrUnknown,
			inStages("", StageStalled),
		),
		Apply: func(_ *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			ps.Stage = StageCIPending
			return nil, nil
		},
	},
	{
		Name:  "ci-pending/noop",
		Guard: ciPendingOrUnknown,
		Apply: func(_ *Controller, _ *PRPipelineState, _ *PRStatus, _ []*run.State) ([]Action, []ActionDescriptor) {
			return nil, nil
		},
	},
}

// ── Guard functions ─────────────────────────────────────────────────────

type guardFunc = func(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) bool

// allOf combines multiple guards with logical AND.
func allOf(guards ...guardFunc) guardFunc {
	return func(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) bool {
		for _, g := range guards {
			if !g(c, ps, status, runStates) {
				return false
			}
		}
		return true
	}
}

// CI status guards.

func ciFailing(_ *Controller, _ *PRPipelineState, status *PRStatus, _ []*run.State) bool {
	return status.CI == "failing"
}

func ciPassing(_ *Controller, _ *PRPipelineState, status *PRStatus, _ []*run.State) bool {
	return status.CI == "passing"
}

func ciPendingOrUnknown(_ *Controller, _ *PRPipelineState, status *PRStatus, _ []*run.State) bool {
	return status.CI != "failing" && status.CI != "passing"
}

// Agent status guards.

func agentNotRunning(_ *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) bool {
	return !ps.AgentRunning
}

func cooldownExpired(_ *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) bool {
	return time.Since(ps.LastDispatchAt) > dispatchCooldown
}

// Fix attempt guards.

func fixAttemptsExhausted(_ *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) bool {
	return ps.FixAttempts >= maxFixAttempts
}

// Stage guards.

func inStage(s Stage) guardFunc {
	return func(_ *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) bool {
		return ps.Stage == s
	}
}

func inStages(stages ...Stage) guardFunc {
	return func(_ *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) bool {
		for _, s := range stages {
			if ps.Stage == s {
				return true
			}
		}
		return false
	}
}

func notInStages(stages ...Stage) guardFunc {
	return func(_ *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) bool {
		for _, s := range stages {
			if ps.Stage == s {
				return false
			}
		}
		return true
	}
}

// notInStageWhileRunning returns true unless the PR is already in the given
// stage AND the agent is currently running (meaning we're already handling it).
func notInStageWhileRunning(s Stage) guardFunc {
	return func(_ *Controller, ps *PRPipelineState, _ *PRStatus, _ []*run.State) bool {
		return !(ps.Stage == s && ps.AgentRunning)
	}
}

// Review decision guards.

func isApproved(c *Controller, ps *PRPipelineState, status *PRStatus, runStates []*run.State) bool {
	cr := strings.EqualFold(status.ReviewDecision, "CHANGES_REQUESTED")
	return strings.EqualFold(status.ReviewDecision, "APPROVED") ||
		(!cr && c.hasKlausApproval(ps.PRNumber, runStates))
}

func changesRequested(_ *Controller, _ *PRPipelineState, status *PRStatus, _ []*run.State) bool {
	return strings.EqualFold(status.ReviewDecision, "CHANGES_REQUESTED")
}

func hasTrustedComments(_ *Controller, _ *PRPipelineState, status *PRStatus, _ []*run.State) bool {
	return status.HasNewTrustedComments
}

// Conflict guards.

func hasConflicts(_ *Controller, _ *PRPipelineState, status *PRStatus, _ []*run.State) bool {
	return status.Conflicts == "yes"
}

func noConflicts(_ *Controller, _ *PRPipelineState, status *PRStatus, _ []*run.State) bool {
	return status.Conflicts != "yes"
}

// Controller config guards.

func autoMergeEnabled(c *Controller, _ *PRPipelineState, _ *PRStatus, _ []*run.State) bool {
	return c.autoMergeOnApproval
}

// ── Shared helpers used by Apply functions ──────────────────────────────

// emitCIPassedIfNeeded emits the CI-passed event when transitioning from
// stages where CI was not previously known to be passing.
func emitCIPassedIfNeeded(c *Controller, ps *PRPipelineState, status *PRStatus) {
	if ps.Stage == StageCIFailed || ps.Stage == StageCIPending || ps.Stage == StageReviewPending || ps.Stage == StageNeedsRebase {
		c.emitEvent(ps.PRNumber, event.AgentCIPassed, map[string]interface{}{
			"pr_number": ps.PRNumber,
			"pr_url":    status.PRURL,
		})
	}
}

// setApprovedIfNeeded transitions to StageApproved and emits the approval
// event if not already in an approved/merging/rebase stage.
func setApprovedIfNeeded(c *Controller, ps *PRPipelineState, status *PRStatus) {
	if ps.Stage != StageApproved && ps.Stage != StageMerging && ps.Stage != StageNeedsRebase {
		ps.Stage = StageApproved
		c.emitEvent(ps.PRNumber, event.PRApproved, map[string]interface{}{
			"pr_number": ps.PRNumber,
			"pr_url":    status.PRURL,
		})
	}
}

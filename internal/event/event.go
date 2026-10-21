package event

import "time"

// Event represents a single event in the klaus event log.
type Event struct {
	Timestamp string                 `json:"timestamp"`      // RFC3339
	RunID     string                 `json:"run_id"`         // Run that produced the event
	Type      string                 `json:"type"`           // e.g. "agent:completed"
	Data      map[string]interface{} `json:"data,omitempty"` // Type-specific payload
}

// Supported event types.
const (
	ConsultStarted      = "consult:started"
	ConsultCompleted    = "consult:completed"
	ReviewStarted       = "review:started"
	ReviewCompleted     = "review:completed"
	AgentStarted        = "agent:started"
	AgentCompleted      = "agent:completed"
	AgentPRCreated      = "agent:pr-created"
	AgentPaused         = "agent:paused"
	AgentResumed        = "agent:resumed"
	AgentCIPassed       = "agent:ci-passed"
	AgentCIFailed       = "agent:ci-failed"
	AgentNeedsAttention = "agent:needs-attention"
	PRAwaitingApproval  = "pr:awaiting-approval"
	PRApproved          = "pr:approved"
	PRMerged            = "pr:merged"
	// PRApprovalChanged signals that an external decision about a PR's
	// merge-readiness has changed (e.g. `klaus approve` flipped an internal
	// approval flag). It is an invalidation signal — the dashboard reacts by
	// re-fetching GitHub status and re-evaluating the pipeline FSM, just as
	// it does for a real webhook. The name is deliberately backend-agnostic
	// so the same signal can be reused once klaus supports non-GitHub
	// merge-readiness sources.
	PRApprovalChanged = "pr:approval-changed"
	// PipelineStalled signals that the pipeline gave up on a PR after
	// exhausting its retry budget (fix, rebase, review-fix or auto-merge
	// attempts). It is emitted once per entry into the stalled stage, so a
	// coordinator watching the channel gets one loud notification instead of
	// a per-poll stream, and knows the PR now needs a human.
	PipelineStalled = "pipeline:stalled"
	// AgentBranchPushed signals that a run finished without a PR but with its
	// branch on origin — the normal finish in a direct-push repo, where the
	// operator integrates the branch by hand. It carries repo, branch and sha.
	AgentBranchPushed = "agent:branch-pushed"
	// AgentChangeReady signals that finalize marked a pushed branch's head
	// ready on the valley its repo delivers to, as a version of the run's
	// change. It follows agent:branch-pushed and carries repo, change_id,
	// handle, version, title and head.
	AgentChangeReady = "agent:change-ready"
	// PhaseStarted and PhaseCompleted bracket a phase of a run (Phase*
	// below). phase:completed carries the phase's started_at, ended_at and
	// duration_ms, so a reader needn't pair it with its start.
	PhaseStarted   = "phase:started"
	PhaseCompleted = "phase:completed"
	// CheckStarted and CheckCompleted bracket a check an agent ran where
	// klaus can see it: `klaus _pre-review`, or a build, test or lint
	// command in the agent's stream (Check* kinds below). check:completed
	// carries duration_ms and, when known, success.
	CheckStarted   = "check:started"
	CheckCompleted = "check:completed"
)

// The phases of a run. Setup is launch's work before the agent starts (fetch,
// worktree, hooks, pane); it has no phase:started, since a launch that fails
// records no run. Agent is the agent's own work, from its pane starting to
// finalize starting. Finalize is _finalize's work, up to killing the pane.
const (
	PhaseSetup    = "setup"
	PhaseAgent    = "agent"
	PhaseFinalize = "finalize"
)

// The kinds of check:started and check:completed.
const (
	CheckPreReview = "pre-review"
	CheckBuild     = "build"
	CheckTest      = "test"
	CheckLint      = "lint"
)

// Dispatch reasons: why a run was launched, recorded on the run
// (dispatch_reason) and on its events. `klaus launch --reason` names any
// other, such as a timer's "docs-timer".
const (
	DispatchLaunch           = "launch"             // an operator or coordinator launch of new work
	DispatchPRFollowUp       = "pr-follow-up"       // klaus launch --pr
	DispatchChangeFollowUp   = "change-follow-up"   // klaus launch --change
	DispatchResume           = "resume"             // klaus launch --resume-from, without --pr or --change
	DispatchBudgetResume     = "budget-resume"      // --pr on a budget-paused PR
	DispatchCIFix            = "ci-fix"             // the pipeline, for failing CI
	DispatchReviewFix        = "review-fix"         // the pipeline, for changes requested
	DispatchReviewCommentFix = "review-comment-fix" // the pipeline, for a trusted reviewer's comments
	DispatchConflictFix      = "conflict-fix"       // the pipeline, for merge conflicts
)

// BudgetPausedLabel is the GitHub label applied to PRs whose agents have
// paused due to budget exhaustion. Klaus uses the label as the persistence
// signal for the paused state; the draft PR + label is the canonical record.
const BudgetPausedLabel = "klaus:budget-paused"

// New creates an Event with the current timestamp.
func New(runID, eventType string, data map[string]interface{}) Event {
	return Event{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		RunID:     runID,
		Type:      eventType,
		Data:      data,
	}
}

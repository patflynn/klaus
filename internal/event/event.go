package event

import "time"

// Event represents a single event in the klaus event log.
type Event struct {
	Timestamp string                 `json:"timestamp"`       // RFC3339
	RunID     string                 `json:"run_id"`          // Run that produced the event
	Type      string                 `json:"type"`            // e.g. "agent:completed"
	Data      map[string]interface{} `json:"data,omitempty"`  // Type-specific payload
}

// Supported event types.
const (
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

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
	AgentCIPassed       = "agent:ci-passed"
	AgentCIFailed       = "agent:ci-failed"
	AgentNeedsAttention = "agent:needs-attention"
	PRAwaitingApproval  = "pr:awaiting-approval"
	PRApproved          = "pr:approved"
	PRMerged            = "pr:merged"
)

// New creates an Event with the current timestamp.
func New(runID, eventType string, data map[string]interface{}) Event {
	return Event{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		RunID:     runID,
		Type:      eventType,
		Data:      data,
	}
}

package run

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/patflynn/klaus/internal/tmux"
)

// State represents the persistent state of a single agent run.
type State struct {
	ID         string   `json:"id"`
	Prompt     string   `json:"prompt"`
	Issue      *string  `json:"issue"`
	PR         *string  `json:"pr,omitempty"`
	Branch     string   `json:"branch"`
	Worktree   string   `json:"worktree"`
	TmuxPane   *string  `json:"tmux_pane"`
	Budget     *string  `json:"budget"`
	LogFile    *string  `json:"log_file"`
	CreatedAt  string   `json:"created_at"`
	CostUSD    *float64 `json:"cost_usd"`
	DurationMS *int64   `json:"duration_ms"`
	PRURL      *string  `json:"pr_url"`
	Type       string   `json:"type,omitempty"`
	TargetRepo *string  `json:"target_repo,omitempty"`
	CloneDir   *string  `json:"clone_dir,omitempty"`
	Host           *string  `json:"host,omitempty"`
	MergedAt       *string  `json:"merged_at,omitempty"`
	DashboardPane  *string  `json:"dashboard_pane,omitempty"`
	Approved       *bool    `json:"approved,omitempty"`
	ApprovedAt     *string  `json:"approved_at,omitempty"`
	SessionName      *string  `json:"session_name,omitempty"`       // claude -n name, same as run ID
	OriginalRunID    *string  `json:"original_run_id,omitempty"`   // run ID this was forked from
	ClaudeSessionID  *string  `json:"claude_session_id,omitempty"` // Claude conversation UUID for --resume
	RepoRoot         *string  `json:"repo_root,omitempty"`         // absolute path to base repo for worktree recreation
}

// Tmux dependency injection for testing.
var (
	PaneExists = tmux.PaneExists
	PaneIsIdle = tmux.PaneIsIdle
	PaneIsDead = tmux.PaneIsDead
)

// IsAgentRunning checks if the agent's tmux pane is still active and
// executing its command pipeline.
func (s *State) IsAgentRunning() bool {
	if s.TmuxPane == nil || !PaneExists(*s.TmuxPane) {
		return false
	}

	// Finalized runs (cost/duration set) are running only if their pane
	// is not idle (e.g. still showing output before the user closes it).
	if s.CostUSD != nil || s.DurationMS != nil {
		return !PaneIsIdle(*s.TmuxPane)
	}

	// Active (unfinalized) runs are running unless the pane is explicitly dead.
	return !PaneIsDead(*s.TmuxPane)
}

// StaleGracePeriod is how long after creation before a run can be considered stale.
// This allows time for the agent pipeline to start up.
var StaleGracePeriod = 2 * time.Minute

// IsStale returns true if the run appears to have been orphaned — its pipeline
// never ran _finalize. A run is stale when it has no finalization data (CostUSD
// and DurationMS are both nil), its tmux pane no longer exists, and enough time
// has passed since creation to rule out normal startup delays.
func (s *State) IsStale() bool {
	// Already finalized — not stale.
	if s.CostUSD != nil || s.DurationMS != nil {
		return false
	}

	// Sessions are not agent runs.
	if s.Type == "session" {
		return false
	}

	// If the pane reference is nil or the pane still exists, not stale.
	if s.TmuxPane == nil {
		// No pane reference at all — could be stale if old enough, but we
		// require the pane to have existed and then disappeared.
		return false
	}
	if PaneExists(*s.TmuxPane) {
		return false
	}

	// Grace period: don't mark very recent runs as stale.
	created, err := time.Parse(time.RFC3339, s.CreatedAt)
	if err != nil {
		return false
	}
	return time.Since(created) > StaleGracePeriod
}

// GenID generates a run ID in the format YYYYMMDD-HHMM-XXXX where XXXX is 4 hex chars.
func GenID() (string, error) {
	ts := time.Now().Format("20060102-1504")
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random bytes: %w", err)
	}
	return fmt.Sprintf("%s-%s", ts, hex.EncodeToString(b)), nil
}

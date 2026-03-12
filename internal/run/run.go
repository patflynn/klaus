package run

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
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

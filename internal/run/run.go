package run

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// State represents the persistent state of a single agent run.
type State struct {
	ID         string   `json:"id"`
	Prompt     string   `json:"prompt"`
	Issue      *string  `json:"issue"`
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

// StateDir returns the path to the runs state directory inside .git/klaus/runs/.
// It uses git's common dir so it works from worktrees too.
func StateDir(gitCommonDir string) string {
	return filepath.Join(gitCommonDir, "klaus", "runs")
}

// LogDir returns the path to the logs directory inside .git/klaus/logs/.
func LogDir(gitCommonDir string) string {
	return filepath.Join(gitCommonDir, "klaus", "logs")
}

// EnsureDirs creates the state and log directories if they don't exist.
func EnsureDirs(gitCommonDir string) error {
	if err := os.MkdirAll(StateDir(gitCommonDir), 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	if err := os.MkdirAll(LogDir(gitCommonDir), 0o755); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}
	return nil
}

// Save writes the run state to a JSON file in the state directory.
func Save(gitCommonDir string, s *State) error {
	dir := StateDir(gitCommonDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	path := filepath.Join(dir, s.ID+".json")
	return os.WriteFile(path, data, 0o644)
}

// Load reads a run state from its JSON file.
func Load(gitCommonDir, id string) (*State, error) {
	path := filepath.Join(StateDir(gitCommonDir), id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading state file: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing state file: %w", err)
	}
	return &s, nil
}

// List returns all run states, sorted by creation time (newest first).
func List(gitCommonDir string) ([]*State, error) {
	dir := StateDir(gitCommonDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading state dir: %w", err)
	}

	var states []*State
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		s, err := Load(gitCommonDir, id)
		if err != nil {
			continue // skip corrupt files
		}
		states = append(states, s)
	}

	sort.Slice(states, func(i, j int) bool {
		return states[i].CreatedAt > states[j].CreatedAt
	})

	return states, nil
}

// Delete removes a run's state file.
func Delete(gitCommonDir, id string) error {
	path := filepath.Join(StateDir(gitCommonDir), id+".json")
	return os.Remove(path)
}

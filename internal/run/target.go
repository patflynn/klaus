package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Target represents a session-level default target repo.
type Target struct {
	Repo string `json:"repo"` // owner/repo format
}

// targetFile returns the path to the target file for a given store base dir.
func targetFile(baseDir string) string {
	return filepath.Join(baseDir, "target.json")
}

// LoadTarget reads the session-level default target repo.
// Returns ("", nil) if no target is set.
func LoadTarget(baseDir string) (string, error) {
	data, err := os.ReadFile(targetFile(baseDir))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading target file: %w", err)
	}
	var t Target
	if err := json.Unmarshal(data, &t); err != nil {
		return "", fmt.Errorf("parsing target file: %w", err)
	}
	return t.Repo, nil
}

// SaveTarget writes a session-level default target repo.
func SaveTarget(baseDir, repo string) error {
	t := Target{Repo: repo}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling target: %w", err)
	}
	return os.WriteFile(targetFile(baseDir), append(data, '\n'), 0o644)
}

// ClearTarget removes the session-level default target repo.
func ClearTarget(baseDir string) error {
	err := os.Remove(targetFile(baseDir))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing target file: %w", err)
	}
	return nil
}

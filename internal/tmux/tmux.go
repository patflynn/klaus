package tmux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// InSession returns true if we're currently inside a tmux session.
func InSession() bool {
	return os.Getenv("TMUX") != ""
}

// SplitWindow creates a new tmux pane by splitting vertically.
// Returns the new pane ID. The command runs in the specified dir.
func SplitWindow(dir, command string) (string, error) {
	args := []string{
		"split-window", "-v", "-d",
		"-P", "-F", "#{pane_id}",
		"-c", dir,
		command,
	}
	out, err := runTmux(args...)
	if err != nil {
		return "", fmt.Errorf("split-window: %w", err)
	}
	return out, nil
}

// SetPaneTitle sets the title of a tmux pane.
func SetPaneTitle(paneID, title string) error {
	_, err := runTmux("select-pane", "-t", paneID, "-T", title)
	return err
}

// RebalanceLayout rebalances the current window's pane layout.
func RebalanceLayout() {
	runTmux("select-layout", "even-vertical")
}

// PaneExists checks if a tmux pane is still alive.
func PaneExists(paneID string) bool {
	out, err := runTmux("list-panes", "-a", "-F", "#{pane_id}")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == paneID {
			return true
		}
	}
	return false
}

// CapturePane captures the visible content of a pane.
// history controls how many lines of scrollback to include.
func CapturePane(paneID string, history int) (string, error) {
	histStr := fmt.Sprintf("-%d", history)
	return runTmux("capture-pane", "-t", paneID, "-p", "-S", histStr)
}

// KillPane kills a tmux pane.
func KillPane(paneID string) error {
	_, err := runTmux("kill-pane", "-t", paneID)
	return err
}

// BuildArgs returns the tmux command arguments for a given operation.
// Exported for testing command construction without actually running tmux.
func BuildArgs(op string, args ...string) []string {
	return append([]string{op}, args...)
}

func runTmux(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

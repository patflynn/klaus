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
// targetPane specifies which pane to split (e.g. from $TMUX_PANE).
func SplitWindow(targetPane, dir, command string) (string, error) {
	args := []string{
		"split-window",
		"-t", targetPane,
		"-v", "-d",
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

// SplitWindowSized creates a new tmux pane with a specified size.
// orientation is "-v" for top/bottom or "-h" for left/right.
// size is passed to tmux's -l flag (e.g. "30%" or "15").
func SplitWindowSized(targetPane, dir, command, orientation, size string) (string, error) {
	args := []string{
		"split-window",
		"-t", targetPane,
		orientation, "-d",
		"-l", size,
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

// RebalanceLayout rebalances the pane layout for the window containing targetPane.
func RebalanceLayout(targetPane string) error {
	if targetPane == "" {
		return fmt.Errorf("targetPane cannot be empty")
	}
	_, err := runTmux("select-layout", "-t", targetPane, "even-vertical")
	return err
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

// SetWindowOption sets a tmux window option for the window containing target pane.
func SetWindowOption(target, option, value string) error {
	_, err := runTmux("set-option", "-w", "-t", target, option, value)
	return err
}

// RenameWindow renames the tmux window containing the target pane.
func RenameWindow(target, name string) error {
	_, err := runTmux("rename-window", "-t", target, name)
	return err
}

// PaneIsIdle checks if a tmux pane's command has finished running.
// Returns true when the pane is dead (command exited) or the only
// running foreground process is a shell — which indicates the
// command pipeline has completed.
func PaneIsIdle(paneID string) bool {
	// Check if the pane is marked dead (command exited, remain-on-exit set)
	out, err := runTmux("display-message", "-t", paneID, "-p", "#{pane_dead}")
	if err == nil && strings.TrimSpace(out) == "1" {
		return true
	}

	// Check the current foreground command in the pane. While the agent's
	// command pipeline is active, pane_current_command will be "claude",
	// "tee", "klaus", etc. Once finished, only the shell remains.
	out, err = runTmux("display-message", "-t", paneID, "-p", "#{pane_current_command}")
	if err != nil {
		return false
	}
	cmd := strings.TrimSpace(out)
	switch cmd {
	case "bash", "zsh", "sh", "fish", "dash":
		return true
	default:
		return false
	}
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

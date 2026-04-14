package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// defaultTimeout is the timeout for tmux commands. These are local IPC calls
// to the tmux server and should complete almost instantly.
const defaultTimeout = 5 * time.Second

// InSession returns true if we're currently inside a tmux session.
func InSession() bool {
	return os.Getenv("TMUX") != ""
}

// SplitWindow creates a new tmux pane by splitting vertically.
// Returns the new pane ID. The command runs in the specified dir.
// targetPane specifies which pane to split (e.g. from $TMUX_PANE).
func SplitWindow(ctx context.Context, targetPane, dir, command string) (string, error) {
	args := []string{
		"split-window",
		"-t", targetPane,
		"-v", "-d",
		"-P", "-F", "#{pane_id}",
		"-c", dir,
		command,
	}
	out, err := runTmux(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("split-window: %w", err)
	}
	return out, nil
}

// SplitWindowSized creates a new tmux pane with a specified size.
// orientation is "-v" for top/bottom or "-h" for left/right.
// size is passed to tmux's -l flag (e.g. "30%" or "15").
func SplitWindowSized(ctx context.Context, targetPane, dir, command, orientation, size string) (string, error) {
	args := []string{
		"split-window",
		"-t", targetPane,
		orientation, "-d",
		"-l", size,
		"-P", "-F", "#{pane_id}",
		"-c", dir,
		command,
	}
	out, err := runTmux(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("split-window: %w", err)
	}
	return out, nil
}

// SetPaneTitle sets the title of a tmux pane.
// Uses select-pane -T which correctly handles titles with spaces,
// unlike set-option -p pane-title which breaks in tmux 3.6+.
func SetPaneTitle(ctx context.Context, paneID, title string) error {
	_, err := runTmux(ctx, "select-pane", "-t", paneID, "-T", title)
	return err
}

// LockPaneTitle prevents applications in the pane from overriding the title.
func LockPaneTitle(ctx context.Context, paneID string) error {
	_, err := runTmux(ctx, "set-option", "-p", "-t", paneID, "allow-rename", "off")
	return err
}

// RebalanceLayout rebalances the pane layout for the window containing targetPane.
func RebalanceLayout(ctx context.Context, targetPane string) error {
	if targetPane == "" {
		return fmt.Errorf("targetPane cannot be empty")
	}
	_, err := runTmux(ctx, "select-layout", "-t", targetPane, "even-vertical")
	return err
}

// SwapPane swaps two tmux panes.
func SwapPane(ctx context.Context, src, dst string) error {
	_, err := runTmux(ctx, "swap-pane", "-s", src, "-t", dst)
	return err
}

// ListWindowPanes returns the pane IDs for the window containing targetPane, in layout order (top to bottom).
func ListWindowPanes(ctx context.Context, targetPane string) ([]string, error) {
	out, err := runTmux(ctx, "list-panes", "-t", targetPane, "-F", "#{pane_id}")
	if err != nil {
		return nil, fmt.Errorf("list-panes: %w", err)
	}
	var panes []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			panes = append(panes, line)
		}
	}
	return panes, nil
}

// PaneExists checks if a tmux pane is still alive.
func PaneExists(ctx context.Context, paneID string) bool {
	out, err := runTmux(ctx, "list-panes", "-a", "-F", "#{pane_id}")
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
func CapturePane(ctx context.Context, paneID string, history int) (string, error) {
	histStr := fmt.Sprintf("-%d", history)
	return runTmux(ctx, "capture-pane", "-t", paneID, "-p", "-S", histStr)
}

// KillPane kills a tmux pane.
func KillPane(ctx context.Context, paneID string) error {
	_, err := runTmux(ctx, "kill-pane", "-t", paneID)
	return err
}

// SetWindowOption sets a tmux window option for the window containing target pane.
func SetWindowOption(ctx context.Context, target, option, value string) error {
	_, err := runTmux(ctx, "set-option", "-w", "-t", target, option, value)
	return err
}

// RenameWindow renames the tmux window containing the target pane.
func RenameWindow(ctx context.Context, target, name string) error {
	_, err := runTmux(ctx, "rename-window", "-t", target, name)
	return err
}

// PaneIsDead checks if a tmux pane's command has exited.
// Only returns true when remain-on-exit is set and the process is gone.
func PaneIsDead(ctx context.Context, paneID string) bool {
	out, err := runTmux(ctx, "display-message", "-t", paneID, "-p", "#{pane_dead}")
	return err == nil && strings.TrimSpace(out) == "1"
}

// PaneIsIdle checks if a tmux pane's command has finished running.
// Returns true when the pane is dead (command exited) or the only
// running foreground process is a shell — which indicates the
// command pipeline has likely completed. Note that this may return
// true for active agents that are currently executing shell scripts.
func PaneIsIdle(ctx context.Context, paneID string) bool {
	if PaneIsDead(ctx, paneID) {
		return true
	}

	// Check the current foreground command in the pane. While the agent's
	// command pipeline is active, pane_current_command will be "claude",
	// "tee", "klaus", etc. Once finished, only the shell remains.
	out, err := runTmux(ctx, "display-message", "-t", paneID, "-p", "#{pane_current_command}")
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

// ensureTimeout returns a context with defaultTimeout applied if the parent
// context has no deadline set. If the parent already has a shorter deadline,
// it is returned unchanged.
func ensureTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}

func runTmux(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("tmux %s timed out after %s", args[0], defaultTimeout)
		}
		return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

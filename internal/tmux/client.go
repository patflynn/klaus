package tmux

import "context"

// Client defines all tmux operations that Klaus uses.
// Implementations can wrap the real tmux binary or provide
// test doubles for unit testing.
type Client interface {
	// InSession returns true if we're currently inside a tmux session.
	InSession() bool

	// SplitWindow creates a new tmux pane by splitting vertically.
	// Returns the new pane ID. The command runs in the specified dir.
	SplitWindow(ctx context.Context, targetPane, dir, command string) (string, error)

	// SplitWindowSized creates a new tmux pane with a specified size.
	// orientation is "-v" for top/bottom or "-h" for left/right.
	// size is passed to tmux's -l flag (e.g. "30%" or "15").
	SplitWindowSized(ctx context.Context, targetPane, dir, command, orientation, size string) (string, error)

	// SetPaneTitle sets the title of a tmux pane.
	SetPaneTitle(ctx context.Context, paneID, title string) error

	// LockPaneTitle prevents applications in the pane from overriding the title.
	LockPaneTitle(ctx context.Context, paneID string) error

	// RebalanceLayout rebalances the pane layout for the window containing targetPane.
	RebalanceLayout(ctx context.Context, targetPane string) error

	// SwapPane swaps two tmux panes.
	SwapPane(ctx context.Context, src, dst string) error

	// ListWindowPanes returns the pane IDs for the window containing targetPane.
	ListWindowPanes(ctx context.Context, targetPane string) ([]string, error)

	// PaneExists checks if a tmux pane is still alive.
	PaneExists(ctx context.Context, paneID string) bool

	// PaneIsDead checks if a tmux pane's command has exited.
	PaneIsDead(ctx context.Context, paneID string) bool

	// PaneIsIdle checks if a tmux pane's command has finished running.
	PaneIsIdle(ctx context.Context, paneID string) bool

	// CapturePane captures the visible content of a pane.
	CapturePane(ctx context.Context, paneID string, history int) (string, error)

	// KillPane kills a tmux pane.
	KillPane(ctx context.Context, paneID string) error

	// SetWindowOption sets a tmux window option for the window containing target pane.
	SetWindowOption(ctx context.Context, target, option, value string) error

	// RenameWindow renames the tmux window containing the target pane.
	RenameWindow(ctx context.Context, target, name string) error
}

// ExecClient implements Client by shelling out to the tmux binary.
type ExecClient struct{}

// NewExecClient returns a Client backed by the real tmux binary.
func NewExecClient() *ExecClient { return &ExecClient{} }

func (c *ExecClient) InSession() bool { return InSession() }

func (c *ExecClient) SplitWindow(ctx context.Context, targetPane, dir, command string) (string, error) {
	return SplitWindow(targetPane, dir, command)
}

func (c *ExecClient) SplitWindowSized(ctx context.Context, targetPane, dir, command, orientation, size string) (string, error) {
	return SplitWindowSized(targetPane, dir, command, orientation, size)
}

func (c *ExecClient) SetPaneTitle(ctx context.Context, paneID, title string) error {
	return SetPaneTitle(paneID, title)
}

func (c *ExecClient) LockPaneTitle(ctx context.Context, paneID string) error {
	return LockPaneTitle(paneID)
}

func (c *ExecClient) RebalanceLayout(ctx context.Context, targetPane string) error {
	return RebalanceLayout(targetPane)
}

func (c *ExecClient) SwapPane(ctx context.Context, src, dst string) error {
	return SwapPane(src, dst)
}

func (c *ExecClient) ListWindowPanes(ctx context.Context, targetPane string) ([]string, error) {
	return ListWindowPanes(targetPane)
}

func (c *ExecClient) PaneExists(ctx context.Context, paneID string) bool {
	return PaneExists(paneID)
}

func (c *ExecClient) PaneIsDead(ctx context.Context, paneID string) bool {
	return PaneIsDead(paneID)
}

func (c *ExecClient) PaneIsIdle(ctx context.Context, paneID string) bool {
	return PaneIsIdle(paneID)
}

func (c *ExecClient) CapturePane(ctx context.Context, paneID string, history int) (string, error) {
	return CapturePane(paneID, history)
}

func (c *ExecClient) KillPane(ctx context.Context, paneID string) error {
	return KillPane(paneID)
}

func (c *ExecClient) SetWindowOption(ctx context.Context, target, option, value string) error {
	return SetWindowOption(target, option, value)
}

func (c *ExecClient) RenameWindow(ctx context.Context, target, name string) error {
	return RenameWindow(target, name)
}

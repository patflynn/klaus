package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

var (
	version = "dev"
)

var rootCmd = &cobra.Command{
	Use:   "klaus",
	Short: "Multi-agent orchestrator for Claude Code, Codex, and agy",
	Long: `klaus orchestrates parallel coding agents using git worktrees and tmux panes.

It launches autonomous agents in isolated worktrees, manages their lifecycle,
streams and formats their output, and tracks run state.

Running 'klaus' with no arguments resumes the most recent coordinator session.
If no session exists, a new one is created. Use 'klaus new' to explicitly start
a fresh session.`,
	Version: version,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSession(cmd, false)
	},
}

func Execute() error {
	return rootCmd.Execute()
}

// exitError is an error that sets klaus's exit status.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// ExitCode is the exit status for an error Execute returned: the one the
// error asks for, else 1.
func ExitCode(err error) int {
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	return 1
}

func init() {
	rootCmd.SetVersionTemplate(fmt.Sprintf("klaus version %s\n", version))
}

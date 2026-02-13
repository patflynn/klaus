package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	version = "dev"
)

var rootCmd = &cobra.Command{
	Use:   "klaus",
	Short: "Multi-agent orchestrator for Claude Code",
	Long: `klaus orchestrates parallel Claude Code agents using git worktrees and tmux panes.

It launches autonomous agents in isolated worktrees, manages their lifecycle,
streams and formats their output, and tracks run state.`,
	Version: version,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.SetVersionTemplate(fmt.Sprintf("klaus version %s\n", version))
}

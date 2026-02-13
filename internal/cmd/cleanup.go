package cmd

import (
	"fmt"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup <run-id> | --all",
	Short: "Remove worktrees, panes, and state",
	Long: `Cleans up a run by killing its tmux pane, removing its worktree,
deleting local branches, and removing state files.

Use --all to clean up all runs.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")

		root, err := git.RepoRoot()
		if err != nil {
			return fmt.Errorf("not inside a git repository")
		}

		commonDir, err := git.CommonDir()
		if err != nil {
			return err
		}

		if all {
			return cleanupAll(root, commonDir)
		}

		if len(args) != 1 {
			return fmt.Errorf("usage: klaus cleanup <run-id> or klaus cleanup --all")
		}
		return cleanupOne(root, commonDir, args[0])
	},
}

func cleanupAll(root, commonDir string) error {
	states, err := run.List(commonDir)
	if err != nil {
		return err
	}
	if len(states) == 0 {
		fmt.Println("No runs to clean up.")
		return nil
	}
	for _, s := range states {
		if err := cleanupOne(root, commonDir, s.ID); err != nil {
			fmt.Printf("  warning: failed to clean up %s: %v\n", s.ID, err)
		}
	}
	return nil
}

func cleanupOne(root, commonDir, id string) error {
	state, err := run.Load(commonDir, id)
	if err != nil {
		return fmt.Errorf("no run found with id: %s", id)
	}

	fmt.Printf("Cleaning up %s...\n", id)

	// Kill tmux pane if alive
	if state.TmuxPane != nil && tmux.PaneExists(*state.TmuxPane) {
		if err := tmux.KillPane(*state.TmuxPane); err == nil {
			fmt.Println("  killed tmux pane")
		}
	}

	// Remove worktree
	if state.Worktree != "" {
		if err := git.WorktreeRemove(root, state.Worktree); err == nil {
			fmt.Println("  removed worktree")
		}
	}

	// Delete local branch
	if state.Branch != "" {
		if err := git.BranchDelete(root, state.Branch); err == nil {
			fmt.Println("  deleted local branch")
		}
	}

	// Remove state file
	if err := run.Delete(commonDir, id); err == nil {
		fmt.Println("  removed state file")
	}

	fmt.Println("  done.")
	return nil
}

func init() {
	cleanupCmd.Flags().Bool("all", false, "Clean up all runs")
	rootCmd.AddCommand(cleanupCmd)
}

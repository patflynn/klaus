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

		store := run.NewGitDirStore(commonDir)

		if all {
			return cleanupAll(root, store)
		}

		if len(args) != 1 {
			return fmt.Errorf("usage: klaus cleanup <run-id> or klaus cleanup --all")
		}
		return cleanupOne(root, store, args[0])
	},
}

func cleanupAll(root string, store run.StateStore) error {
	states, err := store.List()
	if err != nil {
		return err
	}
	if len(states) == 0 {
		fmt.Println("No runs to clean up.")
		return nil
	}
	for _, s := range states {
		if err := cleanupOne(root, store, s.ID); err != nil {
			fmt.Printf("  warning: failed to clean up %s: %v\n", s.ID, err)
		}
	}
	return nil
}

func cleanupOne(root string, store run.StateStore, id string) error {
	state, err := store.Load(id)
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

	// For cross-repo runs, git ops target the clone dir instead of the host root
	gitRoot := root
	if state.CloneDir != nil {
		gitRoot = *state.CloneDir
	}

	// Remove worktree
	if state.Worktree != "" {
		if err := git.WorktreeRemove(gitRoot, state.Worktree); err == nil {
			fmt.Println("  removed worktree")
		}
	}

	// Delete local branch
	if state.Branch != "" {
		if err := git.BranchDelete(gitRoot, state.Branch); err == nil {
			fmt.Println("  deleted local branch")
		}
	}

	// Remove state file
	if err := store.Delete(id); err == nil {
		fmt.Println("  removed state file")
	}

	fmt.Println("  done.")
	return nil
}

func init() {
	cleanupCmd.Flags().Bool("all", false, "Clean up all runs")
	rootCmd.AddCommand(cleanupCmd)
}

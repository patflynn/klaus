package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"

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

Use --all to clean up all runs. Runs with active tmux panes are skipped
by default; pass --force to remove them anyway.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		force, _ := cmd.Flags().GetBool("force")

		root, _ := git.RepoRoot() // may be empty outside a repo
		gitClient := git.NewExecClient()

		deps := DefaultCleanupDeps()

		if all {
			store, err := sessionStoreOrAll()
			if err != nil {
				return err
			}
			if store != nil {
				return cleanupAll(root, store, gitClient, force, deps)
			}
			// No session env — could scan all, but require explicit session
			return fmt.Errorf("KLAUS_SESSION_ID not set; specify a run ID or run inside a session")
		}

		if len(args) != 1 {
			return fmt.Errorf("usage: klaus cleanup <run-id> or klaus cleanup --all")
		}

		state, store, err := loadStateFromEnvOrAll(args[0])
		if err != nil {
			return err
		}
		_ = state // cleanupOne will re-load
		return cleanupOne(root, store, gitClient, args[0], force, deps)
	},
}

func cleanupAll(root string, store run.StateStore, gitClient git.Client, force bool, deps CleanupDeps) error {
	states, err := store.List()
	if err != nil {
		return err
	}
	if len(states) == 0 {
		fmt.Println("No runs to clean up.")
		return nil
	}
	for _, s := range states {
		if err := cleanupOne(root, store, gitClient, s.ID, force, deps); err != nil {
			fmt.Printf("  warning: failed to clean up %s: %v\n", s.ID, err)
		}
	}
	return nil
}

// CleanupDeps holds dependencies for cleanup operations.
type CleanupDeps struct {
	IsRunActive func(state *run.State) bool
}

// DefaultCleanupDeps returns CleanupDeps wired to real implementations.
func DefaultCleanupDeps() CleanupDeps {
	return CleanupDeps{
		IsRunActive: defaultIsRunActive,
	}
}

// defaultIsRunActive reports whether the run has a live, non-idle tmux pane or is the current session.
func defaultIsRunActive(state *run.State) bool {
	if state.Type == "session" {
		if sid := os.Getenv(sessionIDEnv); sid != "" && state.ID == sid {
			return true
		}
	}

	if state.IsAgentRunning() {
		return true
	}

	if state.DashboardPane != nil && tmux.PaneExists(*state.DashboardPane) {
		return true
	}
	return false
}

func cleanupOne(root string, store run.StateStore, gitClient git.Client, id string, force bool, deps CleanupDeps) error {
	state, err := store.Load(id)
	if err != nil {
		return fmt.Errorf("no run found with id: %s", id)
	}

	if !force && deps.IsRunActive(state) {
		fmt.Printf("skipping %s (still running) — use --force to remove\n", id)
		return nil
	}

	fmt.Printf("Cleaning up %s...\n", id)

	ctx := context.TODO()

	// Kill tmux pane if alive
	if state.TmuxPane != nil && tmux.PaneExists(*state.TmuxPane) {
		if err := tmux.KillPane(*state.TmuxPane); err == nil {
			fmt.Println("  killed tmux pane")
		} else {
			slog.Warn("failed to kill tmux pane", "id", id, "pane", *state.TmuxPane, "err", err)
		}
	}

	// Kill dashboard pane if alive
	if state.DashboardPane != nil && tmux.PaneExists(*state.DashboardPane) {
		if err := tmux.KillPane(*state.DashboardPane); err == nil {
			fmt.Println("  killed dashboard pane")
		} else {
			slog.Warn("failed to kill dashboard pane", "id", id, "pane", *state.DashboardPane, "err", err)
		}
	}

	// For cross-repo runs, git ops target the clone dir instead of the host root
	gitRoot := root
	if state.CloneDir != nil {
		gitRoot = *state.CloneDir
	}

	// Remove worktree
	if state.Worktree != "" {
		if err := gitClient.WorktreeRemove(ctx, gitRoot, state.Worktree); err == nil {
			fmt.Println("  removed worktree")
		} else {
			slog.Warn("failed to remove worktree", "id", id, "worktree", state.Worktree, "err", err)
		}
	}

	// Delete local branch
	if state.Branch != "" {
		if err := gitClient.BranchDelete(ctx, gitRoot, state.Branch); err == nil {
			fmt.Println("  deleted local branch")
		} else {
			slog.Warn("failed to delete local branch", "id", id, "branch", state.Branch, "err", err)
		}
	}

	// Remove state file
	if err := store.Delete(id); err == nil {
		fmt.Println("  removed state file")
	} else {
		slog.Warn("failed to remove state file", "id", id, "err", err)
	}

	fmt.Println("  done.")
	return nil
}

func init() {
	cleanupCmd.Flags().Bool("all", false, "Clean up all runs")
	cleanupCmd.Flags().Bool("force", false, "Remove runs even if they are still running")
	rootCmd.AddCommand(cleanupCmd)
}

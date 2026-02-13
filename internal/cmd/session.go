package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Start an interactive coordinator session",
	Long: `Creates an isolated worktree and starts an interactive Claude Code session.
The coordinator runs here instead of the base repo, keeping the base repo
clean on the default branch. Must be run inside a tmux session.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !tmux.InSession() {
			return fmt.Errorf("klaus session must be run inside a tmux session")
		}

		root, err := git.RepoRoot()
		if err != nil {
			return fmt.Errorf("not inside a git repository")
		}

		commonDir, err := git.CommonDir()
		if err != nil {
			return err
		}

		cfg, err := config.Load(root)
		if err != nil {
			return err
		}

		if err := run.EnsureDirs(commonDir); err != nil {
			return err
		}

		baseID, err := run.GenID()
		if err != nil {
			return err
		}
		id := "session-" + baseID
		branch := "session/" + id
		worktree := filepath.Join(cfg.WorktreeBase, id)
		defaultBranch := cfg.DefaultBranch

		fmt.Printf("Creating coordinator session %s...\n", id)

		if err := git.FetchBranch(root, defaultBranch); err != nil {
			return fmt.Errorf("fetching %s: %w", defaultBranch, err)
		}

		startPoint := "origin/" + defaultBranch
		if err := git.WorktreeAdd(root, worktree, branch, startPoint); err != nil {
			return fmt.Errorf("creating worktree: %w", err)
		}
		fmt.Printf("  worktree: %s\n", worktree)
		fmt.Printf("  branch:   %s\n", branch)

		// Write state
		createdAt := time.Now().Format(time.RFC3339)
		state := &run.State{
			ID:        id,
			Type:      "session",
			Prompt:    "(interactive session)",
			Branch:    branch,
			Worktree:  worktree,
			CreatedAt: createdAt,
		}
		if err := run.Save(commonDir, state); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}

		fmt.Println()
		fmt.Println("Starting interactive Claude Code session...")
		fmt.Println("  Use 'klaus launch' from inside to spawn workers.")
		fmt.Println()

		// Run claude interactively in the worktree
		claude := exec.Command("claude")
		claude.Dir = worktree
		claude.Stdin = os.Stdin
		claude.Stdout = os.Stdout
		claude.Stderr = os.Stderr
		claude.Run() // ignore error — user may exit normally

		fmt.Println()
		fmt.Printf("Session %s ended.\n", id)
		fmt.Printf("  Worktree preserved at: %s\n", worktree)
		fmt.Printf("  To clean up: klaus cleanup %s\n", id)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(sessionCmd)
}

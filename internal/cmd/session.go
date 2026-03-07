package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/nix"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

const klausSessionIDEnv = "KLAUS_SESSION_ID"

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Start an interactive coordinator session",
	Long: `Creates an isolated worktree and starts an interactive Claude Code session.
The coordinator runs here instead of the base repo, keeping the base repo
clean on the default branch. Must be run inside a tmux session.

If run outside a git repository, creates a scratch workspace and uses
~/.klaus/config.json for configuration. Use 'klaus launch --repo owner/repo'
from inside the session to target specific repositories.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !tmux.InSession() {
			return fmt.Errorf("klaus session must be run inside a tmux session")
		}

		// Git repo is optional — session can run without one
		root, _ := git.RepoRoot()
		inRepo := root != ""

		cfg, err := config.Load(root)
		if err != nil {
			return err
		}

		baseID, err := run.GenID()
		if err != nil {
			return err
		}
		id := "session-" + baseID

		store, err := run.NewHomeDirStore(id)
		if err != nil {
			return err
		}
		if err := store.EnsureDirs(); err != nil {
			return err
		}

		var branch, repoName, worktree string

		if inRepo {
			branch = "session/" + id
			repoName = filepath.Base(root)
			worktree = filepath.Join(cfg.WorktreeBase, repoName, id)
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

			if err := config.WriteClaudeSettings(worktree, repoName); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write .claude/settings.json: %v\n", err)
			}

			// Set up Nix dev environment if flake.nix exists
			nix.SetupDevEnvironment(worktree)
		} else {
			// No repo — use a scratch workspace
			worktree = filepath.Join(store.BaseDir(), "workspace")
			if err := os.MkdirAll(worktree, 0o755); err != nil {
				return fmt.Errorf("creating scratch workspace: %w", err)
			}

			fmt.Printf("Creating coordinator session %s (no repo)...\n", id)
			fmt.Printf("  workspace: %s\n", worktree)
		}

		if err := config.PreTrustWorktree(worktree); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not pre-trust worktree: %v\n", err)
		}

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
		if err := store.Save(state); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}

		// Load project list for session prompt
		var projectList string
		if reg, loadErr := project.Load(); loadErr == nil {
			projectList = config.FormatProjectList(reg.List())
		}

		// Render session system prompt
		sessionPrompt, err := config.RenderSessionPrompt(root, config.PromptVars{
			RunID:    id,
			Branch:   branch,
			RepoName: repoName,
			Projects: projectList,
		})
		if err != nil {
			return fmt.Errorf("rendering session prompt: %w", err)
		}

		// Configure tmux window for better situational awareness
		currentPane := os.Getenv("TMUX_PANE")
		if currentPane != "" {
			windowTitle := repoName
			if windowTitle == "" {
				windowTitle = "klaus"
			}
			tmux.SetWindowOption(currentPane, "automatic-rename", "off")
			tmux.RenameWindow(currentPane, windowTitle)
			tmux.SetWindowOption(currentPane, "pane-border-status", "top")
			tmux.SetWindowOption(currentPane, "pane-border-format", "#{pane_title}")
		}

		fmt.Println()
		fmt.Println("Starting interactive Claude Code session...")
		fmt.Println("  Use 'klaus launch' from inside to spawn workers.")
		fmt.Println()

		// Run claude interactively in the worktree, passing session ID to children
		claude := exec.Command("claude", "--dangerously-skip-permissions", "--append-system-prompt", sessionPrompt)
		claude.Dir = worktree
		claude.Stdin = os.Stdin
		claude.Stdout = os.Stdout
		claude.Stderr = os.Stderr
		claude.Env = append(os.Environ(), klausSessionIDEnv+"="+id)
		claude.Run() // ignore error — user may exit normally

		fmt.Println()
		fmt.Printf("Session %s ended.\n", id)
		if inRepo {
			fmt.Printf("  Worktree preserved at: %s\n", worktree)
		}
		fmt.Printf("  To clean up: klaus cleanup %s\n", id)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(sessionCmd)
}

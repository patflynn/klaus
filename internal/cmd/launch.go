package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var launchCmd = &cobra.Command{
	Use:   "launch \"<prompt>\" [flags]",
	Short: "Launch an autonomous Claude Code agent",
	Long: `Creates a git worktree, launches Claude Code in autonomous mode in a new
tmux pane, and tracks the run state. Must be run inside a tmux session.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		prompt := args[0]
		issue, _ := cmd.Flags().GetString("issue")
		budget, _ := cmd.Flags().GetString("budget")

		if !tmux.InSession() {
			return fmt.Errorf("klaus launch must be run inside a tmux session")
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

		if budget == "" {
			budget = cfg.DefaultBudget
		}

		if err := run.EnsureDirs(commonDir); err != nil {
			return err
		}

		id, err := run.GenID()
		if err != nil {
			return err
		}

		repoName := filepath.Base(root)
		branch := "agent/" + id
		worktree := filepath.Join(cfg.WorktreeBase, repoName, id)
		defaultBranch := cfg.DefaultBranch

		fmt.Printf("Launching agent %s...\n", id)

		// Fetch latest default branch
		if err := git.FetchBranch(root, defaultBranch); err != nil {
			return fmt.Errorf("fetching %s: %w", defaultBranch, err)
		}

		// Create worktree
		startPoint := "origin/" + defaultBranch
		if err := git.WorktreeAdd(root, worktree, branch, startPoint); err != nil {
			return fmt.Errorf("creating worktree: %w", err)
		}
		fmt.Printf("  worktree: %s\n", worktree)
		fmt.Printf("  branch:   %s\n", branch)

		if err := config.WriteClaudeSettings(worktree, repoName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write .claude/settings.json: %v\n", err)
		}

		// Build system prompt
		sysPrompt, err := config.RenderPrompt(root, config.PromptVars{
			RunID:    id,
			Issue:    issue,
			Branch:   branch,
			RepoName: repoName,
		})
		if err != nil {
			return fmt.Errorf("rendering prompt: %w", err)
		}

		logFile := filepath.Join(run.LogDir(commonDir), id+".jsonl")

		// Build the claude command
		claudeCmd := buildClaudeCommand(sysPrompt, budget, prompt)

		// Build the pane command: run claude, pipe through tee and formatter, then finalize
		selfBin := "klaus" // assumes klaus is in PATH
		paneCmd := fmt.Sprintf(
			"cd %s && %s | tee %s | %s _format-stream; %s _finalize %s; echo ''; echo \"Run %s exited. Press Enter to close.\"; read",
			shellQuote(worktree),
			claudeCmd,
			shellQuote(logFile),
			selfBin,
			selfBin,
			shellQuote(id),
			id,
		)

		// Launch in tmux pane
		paneID, err := tmux.SplitWindow(worktree, paneCmd)
		if err != nil {
			return fmt.Errorf("creating tmux pane: %w", err)
		}

		tmux.SetPaneTitle(paneID, "agent:"+id)
		tmux.RebalanceLayout()

		// Write state
		createdAt := time.Now().Format(time.RFC3339)
		issuePtr := stringPtr(issue)
		budgetPtr := &budget
		logFilePtr := &logFile

		state := &run.State{
			ID:        id,
			Prompt:    prompt,
			Issue:     issuePtr,
			Branch:    branch,
			Worktree:  worktree,
			TmuxPane:  &paneID,
			Budget:    budgetPtr,
			LogFile:   logFilePtr,
			CreatedAt: createdAt,
		}

		if err := run.Save(commonDir, state); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}

		fmt.Printf("  pane:     %s\n", paneID)
		fmt.Printf("  budget:   $%s\n", budget)
		fmt.Printf("  log:      %s\n", logFile)
		fmt.Println()
		fmt.Printf("Agent %s is running. Use 'klaus status' to check progress.\n", id)
		return nil
	},
}

func buildClaudeCommand(sysPrompt, budget, prompt string) string {
	parts := []string{
		"claude", "-p",
		"--dangerously-skip-permissions",
		"--verbose",
		"--output-format", "stream-json",
		"--max-budget-usd", budget,
		"--append-system-prompt", shellQuote(sysPrompt),
		shellQuote(prompt),
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	// Use single quotes, escaping any existing single quotes
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func init() {
	launchCmd.Flags().String("issue", "", "GitHub issue number to reference")
	launchCmd.Flags().String("budget", "", "Max spend in USD (default from config)")
	rootCmd.AddCommand(launchCmd)
}

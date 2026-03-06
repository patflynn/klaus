package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/nix"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var launchCmd = &cobra.Command{
	Use:   "launch \"<prompt>\" [flags]",
	Short: "Launch an autonomous Claude Code agent",
	Long: `Creates a git worktree, launches Claude Code in autonomous mode in a new
tmux pane, and tracks the run state. Must be run inside a tmux session.

Use --repo to launch an agent against a different GitHub repository. The repo
will be cloned (or fetched if already cached) and the agent gets its own
worktree in that clone.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		prompt := args[0]
		issue, _ := cmd.Flags().GetString("issue")
		budget, _ := cmd.Flags().GetString("budget")
		repoRef, _ := cmd.Flags().GetString("repo")

		if !tmux.InSession() {
			return fmt.Errorf("klaus launch must be run inside a tmux session")
		}

		// Host repo — always needed for state tracking
		hostRoot, err := git.RepoRoot()
		if err != nil {
			return fmt.Errorf("not inside a git repository")
		}

		hostCommonDir, err := git.CommonDir()
		if err != nil {
			return err
		}

		hostCfg, err := config.Load(hostRoot)
		if err != nil {
			return err
		}

		if budget == "" {
			budget = hostCfg.DefaultBudget
		}

		if err := run.EnsureDirs(hostCommonDir); err != nil {
			return err
		}

		id, err := run.GenID()
		if err != nil {
			return err
		}

		// Determine the target repo for git operations.
		// When --repo is set, we clone the target and use it for worktree/branch ops.
		// State is always tracked in the host repo.
		var (
			repoRoot      string  // repo dir for git ops (clone or host)
			repoName      string
			defaultBranch string
			targetRepo    *string
			cloneDirPtr   *string
		)

		if repoRef != "" {
			owner, repo, cloneURL, err := git.ParseRepoRef(repoRef)
			if err != nil {
				return fmt.Errorf("parsing repo reference: %w", err)
			}

			cloneDir := filepath.Join(hostCfg.WorktreeBase, ".repos", owner, repo)

			fmt.Printf("Cloning/fetching %s/%s...\n", owner, repo)
			if err := git.EnsureClone(cloneURL, cloneDir); err != nil {
				return fmt.Errorf("cloning %s: %w", repoRef, err)
			}

			repoRoot = cloneDir
			repoName = repo
			cloneDirPtr = &cloneDir
			targetRepo = &repoRef

			// Use target repo config for default_branch if available
			defaultBranch = "main"
			targetCfg, loadErr := config.Load(cloneDir)
			if loadErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not load config from target repo %s: %v\n", repoRef, loadErr)
			} else if targetCfg.DefaultBranch != "" {
				defaultBranch = targetCfg.DefaultBranch
			}
		} else {
			repoRoot = hostRoot
			repoName = filepath.Base(hostRoot)
			defaultBranch = hostCfg.DefaultBranch
		}

		branch := "agent/" + id
		worktree := filepath.Join(hostCfg.WorktreeBase, repoName, id)

		fmt.Printf("Launching agent %s...\n", id)
		if targetRepo != nil {
			fmt.Printf("  target:   %s\n", *targetRepo)
		}

		// Fetch latest default branch
		if err := git.FetchBranch(repoRoot, defaultBranch); err != nil {
			return fmt.Errorf("fetching %s: %w", defaultBranch, err)
		}

		// Create worktree
		startPoint := "origin/" + defaultBranch
		if err := git.WorktreeAdd(repoRoot, worktree, branch, startPoint); err != nil {
			return fmt.Errorf("creating worktree: %w", err)
		}
		fmt.Printf("  worktree: %s\n", worktree)
		fmt.Printf("  branch:   %s\n", branch)

		if err := config.WriteClaudeSettings(worktree, repoName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write .claude/settings.json: %v\n", err)
		}

		// Set up Nix dev environment if flake.nix exists
		nix.SetupDevEnvironment(worktree)

		// Build system prompt (from target repo's .klaus/prompt.md if it exists)
		sysPrompt, err := config.RenderPrompt(repoRoot, config.PromptVars{
			RunID:    id,
			Issue:    issue,
			Branch:   branch,
			RepoName: repoName,
		})
		if err != nil {
			return fmt.Errorf("rendering prompt: %w", err)
		}

		logFile := filepath.Join(run.LogDir(hostCommonDir), id+".jsonl")

		// Build the claude command
		claudeCmd := buildClaudeCommand(sysPrompt, budget, prompt)

		// Build the pane command: run claude, pipe through tee and formatter, then finalize.
		// For cross-repo launches, finalize must run from the host repo context
		// so that state is resolved from the host's .git/klaus/ directory.
		selfBin := "klaus" // assumes klaus is in PATH
		var finalizePrefix string
		if targetRepo != nil {
			finalizePrefix = fmt.Sprintf("cd %s && ", shellQuote(hostRoot))
		}
		paneCmd := fmt.Sprintf(
			"cd %s && %s | tee %s | %s _format-stream; %s%s _finalize %s; echo ''; echo \"Run %s exited. Press Enter to close.\"; read",
			shellQuote(worktree),
			claudeCmd,
			shellQuote(logFile),
			selfBin,
			finalizePrefix,
			selfBin,
			shellQuote(id),
			id,
		)

		// Launch in tmux pane, targeting the pane that ran this command
		currentPane := os.Getenv("TMUX_PANE")
		paneID, err := tmux.SplitWindow(currentPane, worktree, paneCmd)
		if err != nil {
			return fmt.Errorf("creating tmux pane: %w", err)
		}

		tmux.SetPaneTitle(paneID, FormatPaneTitle(id, issue, prompt))
		if err := tmux.RebalanceLayout(currentPane); err != nil {
			return fmt.Errorf("rebalancing tmux layout: %w", err)
		}

		// Write state
		createdAt := time.Now().Format(time.RFC3339)
		issuePtr := stringPtr(issue)
		budgetPtr := &budget
		logFilePtr := &logFile

		state := &run.State{
			ID:         id,
			Prompt:     prompt,
			Issue:      issuePtr,
			Branch:     branch,
			Worktree:   worktree,
			TmuxPane:   &paneID,
			Budget:     budgetPtr,
			LogFile:    logFilePtr,
			CreatedAt:  createdAt,
			TargetRepo: targetRepo,
			CloneDir:   cloneDirPtr,
		}

		if err := run.Save(hostCommonDir, state); err != nil {
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

// FormatPaneTitle builds a compact pane title for an agent.
// Format: "agent:<short-id> #<issue> <short-desc>"
// Short ID is the last 4 characters of the run ID.
// Short desc is the first 20 characters of the prompt.
func FormatPaneTitle(id, issue, prompt string) string {
	shortID := id
	if len(id) > 4 {
		shortID = id[len(id)-4:]
	}

	title := "agent:" + shortID

	if issue != "" {
		title += " #" + issue
	}

	desc := strings.TrimSpace(prompt)
	if len(desc) > 20 {
		desc = desc[:20]
	}
	if desc != "" {
		title += " " + desc
	}

	return title
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
	launchCmd.Flags().String("repo", "", "Target GitHub repo (e.g., owner/repo or full URL)")
	rootCmd.AddCommand(launchCmd)
}

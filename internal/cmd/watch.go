package cmd

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var watchCmd = &cobra.Command{
	Use:   "watch <pr-number>",
	Short: "Monitor CI for a PR and fix failures",
	Long: `Spawns an autonomous agent that monitors CI checks for an existing PR.
If a check fails, the agent reads the failure logs, diagnoses the issue,
pushes a fix to the PR branch, and repeats until all checks pass.

Must be run inside a tmux session.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		prNumber := args[0]
		budget, _ := cmd.Flags().GetString("budget")

		if !tmux.InSession() {
			return fmt.Errorf("klaus watch must be run inside a tmux session")
		}

		root, err := git.RepoRoot()
		if err != nil {
			return fmt.Errorf("not inside a git repository")
		}

		cfg, err := config.Load(root)
		if err != nil {
			return err
		}

		if budget == "" {
			budget = cfg.DefaultBudget
		}

		store, err := sessionStore()
		if err != nil {
			return err
		}
		if err := store.EnsureDirs(); err != nil {
			return err
		}

		// Get PR head branch
		prBranch, err := getPRBranch(prNumber)
		if err != nil {
			return fmt.Errorf("getting PR branch: %w", err)
		}

		id, err := run.GenID()
		if err != nil {
			return err
		}

		repoName := filepath.Base(root)
		worktree := filepath.Join(cfg.WorktreeBase, repoName, id)

		fmt.Printf("Watching PR #%s (branch: %s)...\n", prNumber, prBranch)

		// Fetch the PR branch
		if err := git.FetchBranch(root, prBranch); err != nil {
			return fmt.Errorf("fetching %s: %w", prBranch, err)
		}

		// Create worktree tracking the PR branch
		if err := git.WorktreeAddTrack(root, worktree, prBranch); err != nil {
			return fmt.Errorf("creating worktree: %w", err)
		}
		fmt.Printf("  worktree: %s\n", worktree)
		fmt.Printf("  branch:   %s\n", prBranch)

		if err := config.WriteClaudeSettings(worktree, repoName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write .claude/settings.json: %v\n", err)
		}

		// Build system prompt
		sysPrompt, err := config.RenderWatchPrompt(root, config.PromptVars{
			RunID:    id,
			PR:       prNumber,
			Branch:   prBranch,
			RepoName: repoName,
		})
		if err != nil {
			return fmt.Errorf("rendering prompt: %w", err)
		}

		logFile := filepath.Join(store.LogDir(), id+".jsonl")

		// Gather review comments context, filtering to trusted reviewers only
		reviewContext := ""
		owner, repoNameGH, ghErr := gh.GetRepoOwnerAndName()
		if ghErr == nil {
			comments, err := gh.FetchPRReviewComments(owner, repoNameGH, prNumber)
			if err == nil && len(comments) > 0 {
				trusted := buildTrustedSet(cfg, owner, repoNameGH)
				var sb strings.Builder
				sb.WriteString("\n\nExisting PR review comments:\n")
				included := 0
				for _, c := range comments {
					if !trusted[c.User.Login] {
						log.Printf("skipping review comment %d from untrusted user %q", c.ID, c.User.Login)
						continue
					}
					sb.WriteString(fmt.Sprintf("- [comment %d] %s: %s\n", c.ID, c.Path, truncate(c.Body, 200)))
					included++
				}
				if included > 0 {
					reviewContext = sb.String()
				}
			}
		}

		// Check initial conflict status
		conflictStatus := getPRConflicts(prNumber)
		conflictNote := ""
		if conflictStatus == "yes" {
			conflictNote = "\n\nNote: This PR currently has merge conflicts. Please rebase onto origin/main as the first step."
		}

		// Build the claude command
		prompt := fmt.Sprintf(
			"Monitor CI for PR #%s. Check the current CI status, and if any checks have failed, diagnose the failures and push fixes. Check for merge conflicts and rebase if needed. Also check and address any PR review comments. After pushing fixes, reply to addressed review comments. Repeat until all checks pass.%s%s",
			prNumber,
			conflictNote,
			reviewContext,
		)
		claudeCmd := buildClaudeCommand(sysPrompt, budget, prompt)

		// Build the pane command: run claude, pipe through tee and formatter, then finalize
		selfBin := "klaus"
		paneCmd := fmt.Sprintf(
			"cd %s && %s | tee %s | %s _format-stream; %s _finalize %s; echo ''; printf 'Watch %%s (PR #%%s) exited. Press Enter to close.\\n' %s %s; read",
			shellQuote(worktree),
			claudeCmd,
			shellQuote(logFile),
			selfBin,
			selfBin,
			shellQuote(id),
			shellQuote(id),
			shellQuote(prNumber),
		)

		// Launch in tmux pane, targeting the pane that ran this command
		currentPane := os.Getenv("TMUX_PANE")
		paneID, err := tmux.SplitWindow(currentPane, worktree, paneCmd)
		if err != nil {
			return fmt.Errorf("creating tmux pane: %w", err)
		}

		tmux.SetPaneTitle(paneID, "watch #"+prNumber)
		if err := tmux.RebalanceLayout(currentPane); err != nil {
			return fmt.Errorf("rebalancing tmux layout: %w", err)
		}

		// Write state
		createdAt := time.Now().Format(time.RFC3339)
		budgetPtr := &budget
		logFilePtr := &logFile

		state := &run.State{
			ID:        id,
			Prompt:    prompt,
			Branch:    prBranch,
			Worktree:  worktree,
			TmuxPane:  &paneID,
			Budget:    budgetPtr,
			LogFile:   logFilePtr,
			CreatedAt: createdAt,
			Type:      "watch",
		}

		if err := store.Save(state); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}

		fmt.Printf("  pane:     %s\n", paneID)
		fmt.Printf("  budget:   $%s\n", budget)
		fmt.Printf("  log:      %s\n", logFile)
		fmt.Println()
		fmt.Printf("Watch agent %s is monitoring PR #%s. Use 'klaus status' to check progress.\n", id, prNumber)
		return nil
	},
}

// buildTrustedSet returns a set of usernames trusted for review comments.
// It combines configured TrustedReviewers with repo collaborators.
func buildTrustedSet(cfg config.Config, owner, repo string) map[string]bool {
	trusted := make(map[string]bool)
	for _, u := range cfg.TrustedReviewers {
		trusted[u] = true
	}
	collabs, err := gh.FetchCollaborators(owner, repo)
	if err != nil {
		log.Printf("warning: could not fetch collaborators: %v", err)
	} else {
		for _, u := range collabs {
			trusted[u] = true
		}
	}
	return trusted
}

// getPRBranch returns the head branch name for a PR using the gh CLI.
func getPRBranch(prNumber string) (string, error) {
	cmd := exec.Command("gh", "pr", "view", "--json", "headRefName", "-q", ".headRefName", "--", prNumber)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh pr view: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	branch := strings.TrimSpace(stdout.String())
	if branch == "" {
		return "", fmt.Errorf("could not determine branch for PR #%s", prNumber)
	}
	return branch, nil
}

func init() {
	watchCmd.Flags().String("budget", "", "Max spend in USD (default from config)")
	rootCmd.AddCommand(watchCmd)
}

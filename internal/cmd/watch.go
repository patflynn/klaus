package cmd

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/event"
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

After all CI checks pass, the agent waits for new review comments
(default 2 minutes, configurable via --review-wait). If new comments
arrive during the wait, the agent addresses them and re-enters the CI
monitoring loop.

Must be run inside a tmux session.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		prNumber := args[0]
		budget, _ := cmd.Flags().GetString("budget")
		reviewWait, _ := cmd.Flags().GetInt("review-wait")

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

		if !cmd.Flags().Changed("review-wait") {
			reviewWait = cfg.ReviewWaitSecs
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

		// Build the pane command with a review-polling loop.
		// After each agent run, poll for new review comments. If new comments
		// arrive within the wait period, re-enter the agent loop.
		selfBin := "klaus"
		baselineFile := filepath.Join(os.TempDir(), "klaus-review-baseline-"+id+".txt")
		paneCmd := buildWatchPaneCommand(
			tmuxSessionEnvPrefix(), worktree, claudeCmd, logFile,
			selfBin, id, prNumber, baselineFile, reviewWait,
		)

		// Launch in tmux pane, targeting the pane that ran this command
		currentPane := os.Getenv("TMUX_PANE")
		paneID, err := tmux.SplitWindow(currentPane, worktree, paneCmd)
		if err != nil {
			return fmt.Errorf("creating tmux pane: %w", err)
		}

		if err := tmux.SetPaneTitle(paneID, "watch #"+prNumber); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to set pane title: %v\n", err)
		}
		if err := tmux.SetWindowOption(paneID, "automatic-rename", "off"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to disable automatic rename: %v\n", err)
		}
		if err := tmux.LockPaneTitle(paneID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to lock pane title: %v\n", err)
		}
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

		// Emit agent:started event for watch agent
		if hds, ok := store.(*run.HomeDirStore); ok {
			emitEvent(hds.BaseDir(), id, event.AgentStarted, map[string]interface{}{
				"id":     id,
				"prompt": prompt,
			})
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
func getPRBranch(prNumber string, repo ...string) (string, error) {
	args := []string{"pr", "view", "--json", "headRefName", "-q", ".headRefName"}
	if len(repo) > 0 && repo[0] != "" {
		args = append(args, "--repo", repo[0])
	}
	args = append(args, "--", prNumber)
	cmd := exec.Command("gh", args...)
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

// buildWatchPaneCommand constructs the shell command run inside the tmux pane.
// It wraps the agent in a loop: after each agent run, it checks CI status
// (emitting events), polls for new review comments, and re-enters the loop
// if any are found within the wait period.
func buildWatchPaneCommand(envPrefix, worktree, claudeCmd, logFile, selfBin, id, prNumber, baselineFile string, reviewWait int) string {
	waitStr := strconv.Itoa(reviewWait)
	qID := shellQuote(id)
	qPR := shellQuote(prNumber)

	// After each agent cycle, check CI and emit events via _event.
	// gh pr checks exits 0 when all checks pass and non-zero otherwise.
	ciCheck := fmt.Sprintf(
		"PR_URL=$(gh pr view %s --json url -q .url 2>/dev/null); "+
			"if gh pr checks %s --fail-fast 2>/dev/null; then "+
			"%s _event --run-id %s --type agent:ci-passed --data '{\"id\":\"%s\",\"pr_url\":\"'\"$PR_URL\"'\"}'; "+
			"%s _event --run-id %s --type pr:awaiting-approval --data '{\"pr_url\":\"'\"$PR_URL\"'\"}'; "+
			"else "+
			"%s _event --run-id %s --type agent:ci-failed --data '{\"id\":\"%s\",\"pr_url\":\"'\"$PR_URL\"'\"}'; "+
			"fi",
		qPR,
		qPR,
		selfBin, qID, id,
		selfBin, qID,
		selfBin, qID, id,
	)

	// The loop:
	// 1. Save current review comment IDs as baseline
	// 2. Run the Claude agent
	// 3. Check CI status and emit events
	// 4. Poll for new review comments (exits 0 if found, non-zero if timeout)
	// 5. If new comments found, update baseline and loop; otherwise break
	return fmt.Sprintf(
		"%scd %s && "+
			"%s _save-review-baseline %s %s; "+
			"while true; do "+
			"%s | tee %s | %s _format-stream; "+
			"%s; "+
			"%s _poll-reviews %s %s --wait %s || break; "+
			"%s _save-review-baseline %s %s; "+
			"done; "+
			"%s _finalize %s",
		envPrefix,
		shellQuote(worktree),
		// Initial baseline save
		selfBin, qPR, shellQuote(baselineFile),
		// Agent loop body
		claudeCmd, shellQuote(logFile), selfBin,
		// CI status check + event emission
		ciCheck,
		// Poll for new reviews
		selfBin, qPR, shellQuote(baselineFile), waitStr,
		// Update baseline for next iteration
		selfBin, qPR, shellQuote(baselineFile),
		// Finalize
		selfBin, qID,
	)
}

func init() {
	watchCmd.Flags().String("budget", "", "Max spend in USD (default from config)")
	watchCmd.Flags().Int("review-wait", 120, "Seconds to wait for new review comments after CI passes")
	rootCmd.AddCommand(watchCmd)
}

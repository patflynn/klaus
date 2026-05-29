package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

var finalizeWIPCmd = &cobra.Command{
	Use:   "finalize <run-id>",
	Short: "Commit and push a paused agent's work-in-progress",
	Long: `Commits any uncommitted changes in the run's worktree and pushes the
branch, without resuming claude. Use this when you want to save the work
an agent did before it paused but don't want to spend more on agent time.

By default a generic 'WIP from klaus run <id>' commit message is used; pass
--message to provide your own. Pass --open-pr to also open a draft PR.

The run must be in 'paused' or 'completed' state. To resume the agent
instead, use 'klaus resume'. To throw the work away, use 'klaus cleanup'.`,
	Example: `  klaus finalize 20260529-1657-abcd1234
  klaus finalize 20260529-1657-abcd1234 --message "fix auth bug"
  klaus finalize 20260529-1657-abcd1234 --open-pr`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		message, _ := cmd.Flags().GetString("message")
		openPR, _ := cmd.Flags().GetBool("open-pr")
		ctx := cmd.Context()

		store, err := sessionStore()
		if err != nil {
			return err
		}

		state, err := store.Load(id)
		if err != nil {
			return fmt.Errorf("no run found with id: %s", id)
		}

		status := stringValue(state.Status)
		if status != run.StatusPaused && status != run.StatusCompleted {
			return fmt.Errorf("run %s is not paused or completed, current status: %s (use 'klaus cleanup' to discard, or wait until the run finishes)", id, statusOrRunning(status))
		}

		if state.Worktree == "" {
			return fmt.Errorf("run %s has no worktree to finalize (it may already have been cleaned up)", id)
		}

		if message == "" {
			message = fmt.Sprintf("WIP from klaus run %s\n\n%s", id, strings.TrimSpace(state.Prompt))
		}

		committed, err := commitWorkInProgress(ctx, state.Worktree, message)
		if err != nil {
			return err
		}
		if committed {
			fmt.Printf("Created commit on %s.\n", state.Branch)
		} else {
			fmt.Println("No uncommitted changes to commit.")
		}

		if err := pushBranch(ctx, state.Worktree, state.Branch); err != nil {
			return fmt.Errorf("pushing branch: %w", err)
		}
		fmt.Printf("Pushed %s.\n", state.Branch)

		if openPR {
			title := promptTitle(state.Prompt)
			body := fmt.Sprintf("Finalized from klaus run %s after the agent paused. Pushed for human inspection.\n\nRun: %s", id, id)
			prURL, err := createDraftPR(ctx, state.Worktree, title, body)
			if err != nil {
				fmt.Fprintf(cmd.OutOrStderr(), "warning: gh pr create failed: %v\n", err)
			} else if prURL != "" {
				state.PRURL = &prURL
				fmt.Printf("Opened draft PR: %s\n", prURL)
			}
		}

		setStatus(state, run.StatusFinalized)
		state.PausedAt = nil
		state.PauseReason = nil
		if err := store.Save(state); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}

		if hds, ok := store.(*run.HomeDirStore); ok {
			data := map[string]interface{}{
				"id":           id,
				"committed":    committed,
				"opened_draft": openPR && state.PRURL != nil,
			}
			if state.PRURL != nil {
				data["pr_url"] = *state.PRURL
			}
			emitEvent(hds.BaseDir(), id, event.AgentFinalized, data)
		}

		return nil
	},
}

func statusOrRunning(s string) string {
	if s == "" {
		return "running"
	}
	return s
}

// commitWorkInProgress stages all changes in worktree and creates a commit
// with the given message. Returns whether a commit was actually created
// (false when the worktree was already clean).
func commitWorkInProgress(ctx context.Context, worktree, message string) (bool, error) {
	add := exec.CommandContext(ctx, "git", "-C", worktree, "add", "-A")
	if out, err := add.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git add -A: %w: %s", err, string(out))
	}

	// Check if there's anything to commit. `git diff --cached --quiet` exits
	// non-zero when there are staged changes.
	check := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "--cached", "--quiet")
	if err := check.Run(); err == nil {
		return false, nil // clean
	}

	commit := exec.CommandContext(ctx, "git", "-C", worktree, "commit", "-m", message)
	var stderr bytes.Buffer
	commit.Stderr = &stderr
	if err := commit.Run(); err != nil {
		return false, fmt.Errorf("git commit: %w: %s", err, stderr.String())
	}
	return true, nil
}

func pushBranch(ctx context.Context, worktree, branch string) error {
	args := []string{"-C", worktree, "push", "-u", "origin"}
	if branch != "" {
		args = append(args, "HEAD:"+branch)
	} else {
		args = append(args, "HEAD")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func createDraftPR(ctx context.Context, worktree, title, body string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "create", "--draft", "--title", title, "--body", body)
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, string(out))
	}
	// gh prints the PR URL on success
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://github.com/") && strings.Contains(line, "/pull/") {
			return line, nil
		}
	}
	return "", nil
}

// promptTitle takes the first non-empty line of an agent prompt and truncates
// to fit a PR title (~70 chars).
func promptTitle(prompt string) string {
	const maxLen = 70
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > maxLen {
			line = line[:maxLen]
		}
		return line
	}
	return fmt.Sprintf("klaus finalize at %s", time.Now().Format("2006-01-02"))
}

func init() {
	finalizeWIPCmd.Flags().String("message", "", "Commit message (default: 'WIP from klaus run <id>')")
	finalizeWIPCmd.Flags().Bool("open-pr", false, "Open a draft PR after pushing")
	rootCmd.AddCommand(finalizeWIPCmd)
}

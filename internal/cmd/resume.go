package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

// defaultResumeAddBudget is the budget bump applied when 'klaus resume' is
// invoked with no --add-budget or --budget flag. Five dollars buys roughly
// one more full attempt at the same task.
const defaultResumeAddBudget = 5.0

var resumeCmd = &cobra.Command{
	Use:   "resume <run-id>",
	Short: "Resume a paused agent run with extended budget",
	Long: `Resumes a paused agent in its existing worktree using claude --resume.

By default, the agent's budget cap is extended by $5. Use --add-budget to
add a specific amount, or --budget to set an absolute new cap.

The run must be in the 'paused' state (e.g. paused after exceeding its
budget). To save the in-progress work without resuming, use 'klaus
finalize' instead.`,
	Example: `  klaus resume 20260529-1657-abcd1234                    # +$5 budget
  klaus resume 20260529-1657-abcd1234 --add-budget 10    # +$10
  klaus resume 20260529-1657-abcd1234 --budget 20        # absolute $20 cap`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		addBudget, _ := cmd.Flags().GetFloat64("add-budget")
		absoluteBudget, _ := cmd.Flags().GetString("budget")
		ctx := cmd.Context()
		tmuxClient := tmux.NewExecClient()

		if !tmux.InSession() {
			return fmt.Errorf("klaus resume must be run inside a tmux session")
		}

		store, err := sessionStore()
		if err != nil {
			return err
		}

		state, err := store.Load(id)
		if err != nil {
			return fmt.Errorf("no run found with id: %s", id)
		}

		status := stringValue(state.Status)
		if status != run.StatusPaused {
			if status == "" {
				status = "running"
			}
			return fmt.Errorf("run %s is not paused, current status: %s", id, status)
		}

		newBudget, err := computeResumeBudget(state.Budget, addBudget, absoluteBudget)
		if err != nil {
			return err
		}

		// Resolve the Claude session UUID. Without it we can't continue the
		// existing conversation, only fork a new one.
		var resumeSession string
		if state.LogFile != nil {
			resumeSession = ExtractClaudeSessionID(*state.LogFile)
		}
		if resumeSession == "" {
			return fmt.Errorf("could not find claude session ID in log; cannot resume (try 'klaus finalize %s' instead)", id)
		}
		if !claudeSessionExists(resumeSession) {
			return fmt.Errorf("claude session %s no longer exists on disk; cannot resume (try 'klaus finalize %s' instead)", resumeSession, id)
		}

		if state.Worktree == "" {
			return fmt.Errorf("run %s has no worktree to resume into", id)
		}
		if _, err := os.Stat(state.Worktree); err != nil {
			return fmt.Errorf("worktree %s no longer exists: %w", state.Worktree, err)
		}

		// Kill the dormant pane so we can split a fresh one in its place. The
		// pane is idle at a shell prompt after the pipeline finished; reusing
		// it would require fragile send-keys gymnastics.
		killDormantPane(ctx, tmuxClient, state)

		// Resume with a vanilla prompt — the system prompt and full
		// conversation are already in the claude session being resumed.
		resumePrompt := fmt.Sprintf("Continue the work. Budget has been extended to $%s.", newBudget)
		claudeCmd := buildClaudeCommand("", newBudget, resumePrompt, id, resumeSession)

		logFile := ""
		if state.LogFile != nil {
			logFile = *state.LogFile
		} else {
			logFile = store.LogDir() + "/" + id + ".jsonl"
		}

		// The same pipeline as launch — claude → tee → format-stream, then
		// _finalize. _finalize will detect budget exhaustion again if the
		// new cap is also hit and re-pause the run.
		selfBin := "klaus"
		paneCmd := fmt.Sprintf(
			"%scd %s && %s | tee -a %s | %s _format-stream; %s _finalize %s",
			tmuxSessionEnvPrefix(),
			shellQuote(state.Worktree),
			claudeCmd,
			shellQuote(logFile),
			selfBin,
			selfBin,
			shellQuote(id),
		)

		currentPane := os.Getenv("TMUX_PANE")
		paneID, err := tmuxClient.SplitWindow(ctx, currentPane, state.Worktree, paneCmd)
		if err != nil {
			return fmt.Errorf("creating tmux pane: %w", err)
		}
		if err := tmuxClient.SetPaneTitle(ctx, paneID, FormatPaneTitle(id, stringValue(state.Issue), state.Prompt)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to set pane title: %v\n", err)
		}
		if err := tmuxClient.SetWindowOption(ctx, paneID, "automatic-rename", "off"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to disable automatic rename: %v\n", err)
		}
		if err := tmuxClient.LockPaneTitle(ctx, paneID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to lock pane title: %v\n", err)
		}
		if err := tmuxClient.RebalanceLayout(ctx, currentPane); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to rebalance layout: %v\n", err)
		}

		setStatus(state, run.StatusRunning)
		state.PausedAt = nil
		state.PauseReason = nil
		state.Budget = &newBudget
		state.TmuxPane = &paneID
		if err := store.Save(state); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}

		if hds, ok := store.(*run.HomeDirStore); ok {
			emitEvent(hds.BaseDir(), id, event.AgentResumed, map[string]interface{}{
				"id":             id,
				"new_budget_usd": newBudget,
				"add_budget_usd": addBudget,
			})
		}

		fmt.Printf("Resuming agent %s with budget $%s in pane %s.\n", id, newBudget, paneID)
		return nil
	},
}

// computeResumeBudget produces the new budget cap as a string. Precedence:
// --budget (absolute) > --add-budget (relative to current) > default add ($5).
func computeResumeBudget(currentBudget *string, addBudget float64, absoluteBudget string) (string, error) {
	if absoluteBudget != "" {
		if _, err := strconv.ParseFloat(absoluteBudget, 64); err != nil {
			return "", fmt.Errorf("invalid --budget %q: %w", absoluteBudget, err)
		}
		return absoluteBudget, nil
	}

	bump := addBudget
	if bump == 0 {
		bump = defaultResumeAddBudget
	}

	var current float64
	if currentBudget != nil && *currentBudget != "" {
		c, err := strconv.ParseFloat(*currentBudget, 64)
		if err != nil {
			return "", fmt.Errorf("invalid existing budget %q: %w", *currentBudget, err)
		}
		current = c
	}

	return strconv.FormatFloat(current+bump, 'f', 2, 64), nil
}

// killDormantPane kills a paused agent's tmux pane if it still exists. The
// pane sits at an idle shell prompt after _finalize pauses the run, so it's
// safe to kill before spawning the resume pane.
func killDormantPane(ctx context.Context, tc tmux.Client, state *run.State) {
	if state.TmuxPane == nil {
		return
	}
	if !tc.PaneExists(ctx, *state.TmuxPane) {
		return
	}
	if err := tc.KillPane(ctx, *state.TmuxPane); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to kill dormant pane %s: %v\n", *state.TmuxPane, err)
	}
}

func stringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func init() {
	resumeCmd.Flags().Float64("add-budget", 0, "Add this many USD to the existing budget cap (default $5)")
	resumeCmd.Flags().String("budget", "", "Replace the budget cap with this absolute USD value")
	rootCmd.AddCommand(resumeCmd)
}

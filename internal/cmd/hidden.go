package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/draft"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/scan"
	"github.com/patflynn/klaus/internal/stream"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

// budgetPauseRunner is overridable in tests to capture gh/git calls without
// touching the network. Production uses draft.ExecRunner{}.
var budgetPauseRunner draft.Runner = draft.ExecRunner{}

var formatStreamCmd = &cobra.Command{
	Use:    "_format-stream",
	Short:  "Format Claude JSONL stream from stdin",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return stream.FormatStream(os.Stdin, os.Stdout)
	},
}

var finalizeCmd = &cobra.Command{
	Use:    "_finalize <run-id>",
	Short:  "Finalize a completed run",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		store, err := sessionStore()
		if err != nil {
			return nil // silently ignore if session not set
		}

		state, err := store.Load(id)
		if err != nil {
			return nil // silently ignore if state not found
		}

		// Track whether _finalize discovered the PR URL for the first time
		// in this run. We emit agent:pr-created even on budget pause so the
		// pipeline starts tracking the (now-draft) PR.
		hadPRURLBefore := state.PRURL != nil && *state.PRURL != ""

		// Parse log for cost/duration/PR URL and result subtype.
		var resultSubtype string
		if state.LogFile != nil {
			subtype, err := finalizeFromLog(store, state)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: finalize: %v\n", err)
			}
			resultSubtype = subtype
		}

		ctx := cmd.Context()
		baseDir := ""
		if hds, ok := store.(*run.HomeDirStore); ok {
			baseDir = hds.BaseDir()
		}

		// Decide: did this run end normally, or did it exhaust its budget?
		paused := handleBudgetPauseIfNeeded(ctx, baseDir, state, resultSubtype, hadPRURLBefore)

		// Sync to data ref — use the target repo's clone dir if available,
		// otherwise fall back to the current git repo.
		var syncRoot string
		if state.CloneDir != nil {
			syncRoot = *state.CloneDir
		} else {
			syncRoot, err = git.RepoRoot()
			if err != nil {
				return nil
			}
		}

		cfg, err := config.Load(syncRoot)
		if err != nil {
			return nil
		}

		gitClient := git.NewExecClient()

		// For a normal (non-budget, non-crashed) completion against an
		// existing paused PR, clear the budget-paused label so the dashboard
		// reflects that the follow-up agent shipped its work. A crashed
		// follow-up must NOT clear the label — the PR still needs work.
		if !paused && state.FailureReason == nil && baseDir != "" {
			clearLabelIfResumed(ctx, baseDir, state)
		}

		// Emit terminal events for the run. For paused runs, agent:completed
		// is intentionally suppressed in favor of agent:paused, since the
		// run is not "done" — it's parked in a draft PR awaiting continuation.
		// A crashed run emits agent:needs-attention instead of falsely
		// reporting agent:completed / agent:pr-created.
		if baseDir != "" && !paused {
			emitFinalizeEvents(baseDir, state)
		}
		syncRunToDataRef(ctx, syncRoot, store, gitClient, cfg.DataRef, state)

		cleanupWorktree(ctx, store, gitClient, state)

		// Kill the tmux pane — _finalize is the last command in the pipeline,
		// so this is safe. The pane would otherwise stay open indefinitely.
		killAgentPane(ctx, store, tmux.NewExecClient(), state)

		return nil
	},
}

// emitFinalizeEvents emits the terminal events for a finalized, non-paused
// run. A crashed run (FailureReason set) emits agent:needs-attention and
// nothing else, so a pipeline never mistakes a crash for a completed,
// PR-creating run. A normal run emits agent:completed plus agent:pr-created
// when a PR URL is known.
func emitFinalizeEvents(baseDir string, state *run.State) {
	if baseDir == "" || state == nil {
		return
	}
	if state.FailureReason != nil {
		emitEvent(baseDir, state.ID, event.AgentNeedsAttention, map[string]interface{}{
			"id":     state.ID,
			"reason": *state.FailureReason,
		})
		return
	}

	completedData := map[string]interface{}{"id": state.ID}
	if state.CostUSD != nil {
		completedData["cost_usd"] = *state.CostUSD
	}
	if state.DurationMS != nil {
		completedData["duration_ms"] = *state.DurationMS
	}
	emitEvent(baseDir, state.ID, event.AgentCompleted, completedData)

	if state.PRURL != nil && *state.PRURL != "" {
		emitEvent(baseDir, state.ID, event.AgentPRCreated, map[string]interface{}{
			"id":        state.ID,
			"pr_url":    *state.PRURL,
			"pr_number": extractPRNumberFromURL(*state.PRURL),
		})
	}
}

// handleBudgetPauseIfNeeded decides whether the just-finalized run hit its
// budget cap, and if so commits/pushes the WIP, ensures a draft PR with
// the budget-paused label, and emits agent:paused (plus agent:pr-created
// if a PR was newly discovered or created).
//
// Returns true if the budget-pause flow was taken (so the caller can
// suppress the normal agent:completed event).
func handleBudgetPauseIfNeeded(ctx context.Context, baseDir string, state *run.State, resultSubtype string, hadPRURLBefore bool) bool {
	if !isBudgetExhausted(state, resultSubtype) {
		return false
	}
	if state.Worktree == "" {
		// Nothing to push: worktree already cleaned up. Best-effort skip.
		return false
	}

	budgetUSD := 0.0
	if state.Budget != nil {
		if v, err := strconv.ParseFloat(*state.Budget, 64); err == nil {
			budgetUSD = v
		}
	}
	cost := 0.0
	if state.CostUSD != nil {
		cost = *state.CostUSD
	}

	repo := ""
	if state.TargetRepo != nil {
		repo = *state.TargetRepo
	}
	if !strings.Contains(repo, "/") {
		// Bare project name — gh can't use it. Let gh infer from the
		// worktree's remote.
		repo = ""
	}

	existingPR := ""
	if state.PR != nil {
		existingPR = *state.PR
	}

	in := draft.PauseInput{
		RunID:      state.ID,
		Worktree:   state.Worktree,
		Branch:     state.Branch,
		Repo:       repo,
		Prompt:     state.Prompt,
		CostUSD:    cost,
		BudgetUSD:  budgetUSD,
		ExistingPR: existingPR,
	}

	out, err := draft.HandleBudgetPause(ctx, budgetPauseRunner, in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: budget-pause flow failed: %v\n", err)
		// Fall through: emit no agent:paused event, treat as a regular
		// (failed) completion so the caller's normal-event branch fires.
		return false
	}

	// Persist the discovered PR URL so the dashboard picks up the draft PR.
	if out.PRURL != "" {
		state.PRURL = &out.PRURL
	}
	if out.PRNumber != "" {
		pr := out.PRNumber
		state.PR = &pr
	}

	if baseDir != "" {
		data := map[string]interface{}{
			"id":         state.ID,
			"pr_number":  out.PRNumber,
			"pr_url":     out.PRURL,
			"cost_usd":   cost,
			"budget_usd": budgetUSD,
			"reason":     "budget_exhausted",
		}
		emitEvent(baseDir, state.ID, event.AgentPaused, data)

		// If this is the first time klaus has seen the PR URL for this run
		// (either because the agent created it on this final flush, or
		// because we just created it), emit agent:pr-created so the
		// pipeline starts tracking it.
		if !hadPRURLBefore && out.PRURL != "" {
			emitEvent(baseDir, state.ID, event.AgentPRCreated, map[string]interface{}{
				"id":        state.ID,
				"pr_url":    out.PRURL,
				"pr_number": out.PRNumber,
			})
		}
	}
	return true
}

// isBudgetExhausted decides whether the just-completed run terminated
// because it hit the budget cap. The signal is: claude did NOT emit a
// success result event AND observed cost is at least 95% of the budget cap.
//
// When the result event is "success", we trust claude and never treat the
// run as paused even if cost is near cap.
//
// When the result event is absent (early kill, crash, stream truncation)
// OR has a non-success subtype, we apply the 95% heuristic. This avoids
// false positives where cost ramped fast for an unrelated reason.
func isBudgetExhausted(state *run.State, resultSubtype string) bool {
	if state.Budget == nil || state.CostUSD == nil {
		return false
	}
	cap, err := strconv.ParseFloat(*state.Budget, 64)
	if err != nil || cap <= 0 {
		return false
	}
	if resultSubtype == "success" {
		return false
	}
	return draft.BudgetExhausted(*state.CostUSD, cap)
}

// clearLabelIfResumed removes the klaus:budget-paused label if it was set
// on the run's PR, and emits agent:resumed so the dashboard reflects the
// pause being resolved. Called only on successful (non-paused) finalize.
func clearLabelIfResumed(ctx context.Context, baseDir string, state *run.State) {
	if state.PR == nil || *state.PR == "" {
		return
	}
	repo := ""
	if state.TargetRepo != nil && strings.Contains(*state.TargetRepo, "/") {
		repo = *state.TargetRepo
	}
	workdir := state.Worktree
	if workdir == "" && state.CloneDir != nil {
		workdir = *state.CloneDir
	}

	had, err := draft.HasBudgetPausedLabel(ctx, budgetPauseRunner, workdir, repo, *state.PR)
	if err != nil || !had {
		return
	}
	if err := draft.ClearBudgetPausedLabel(ctx, budgetPauseRunner, workdir, repo, *state.PR); err != nil {
		fmt.Fprintf(os.Stderr, "warning: clearing budget-paused label: %v\n", err)
		return
	}

	data := map[string]interface{}{
		"id":        state.ID,
		"pr_number": *state.PR,
	}
	if state.PRURL != nil {
		data["pr_url"] = *state.PRURL
	}
	emitEvent(baseDir, state.ID, event.AgentResumed, data)
}

// cleanupWorktree removes the agent's worktree and local branch after
// completion. The state file and logs are preserved. It is idempotent —
// if the worktree is already gone, the state is still cleared.
func cleanupWorktree(ctx context.Context, store run.StateStore, gitClient git.Client, state *run.State) {
	if state.Worktree == "" {
		return
	}
	gitRoot := ""
	if state.CloneDir != nil {
		gitRoot = *state.CloneDir
	} else {
		gitRoot, _ = git.RepoRoot()
	}
	if gitRoot == "" {
		return
	}
	if err := gitClient.WorktreeRemove(ctx, gitRoot, state.Worktree); err != nil {
		fmt.Fprintf(os.Stderr, "warning: worktree cleanup: %v\n", err)
	}
	if state.Branch != "" {
		if err := gitClient.BranchDelete(ctx, gitRoot, state.Branch); err != nil {
			slog.Warn("failed to delete branch during cleanup", "id", state.ID, "branch", state.Branch, "err", err)
		}
	}
	state.Worktree = ""
	if err := store.Save(state); err != nil {
		slog.Warn("failed to save state after worktree cleanup", "id", state.ID, "err", err)
	}
}

// killAgentPane kills the tmux pane associated with the agent. State is
// saved before the pane is killed because _finalize runs inside the pane
// itself — killing the pane first would terminate the process before the
// state save executes.
func killAgentPane(ctx context.Context, store run.StateStore, tc tmux.Client, state *run.State) {
	if state.TmuxPane == nil {
		return
	}
	paneID := *state.TmuxPane
	state.TmuxPane = nil
	if err := store.Save(state); err != nil {
		slog.Warn("failed to save state before pane cleanup", "id", state.ID, "err", err)
	}
	if err := tc.KillPane(ctx, paneID); err != nil {
		slog.Warn("failed to kill agent pane", "id", state.ID, "pane", paneID, "err", err)
	}
}

// finalizeFromLog parses the agent's JSONL log, mutates state with the
// observed cost / duration / PR URL, and returns the subtype of the last
// "result" event (e.g. "success", "error_max_turns", or empty if no
// result event was emitted). The subtype lets the caller distinguish a
// successful completion from a budget-cap kill.
func finalizeFromLog(store run.StateStore, state *run.State) (string, error) {
	f, err := os.Open(*state.LogFile)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Preserve PRURL set at launch time (e.g. --pr mode).
	// Only extract from logs when no URL is already known; otherwise the
	// regex can clobber the correct value with a false match from agent
	// tool output (source code, test fixtures, etc.).
	existingPRURL := ""
	if state.PRURL != nil {
		existingPRURL = *state.PRURL
	}

	// Use line-by-line scanning for robust JSONL parsing.
	// json.NewDecoder can corrupt its internal state on malformed lines,
	// causing subsequent events to be silently skipped.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	var resultSubtype string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev struct {
			Type         string   `json:"type"`
			Subtype      string   `json:"subtype"`
			SessionID    string   `json:"session_id"`
			TotalCostUSD float64  `json:"total_cost_usd"`
			DurationMS   int64    `json:"duration_ms"`
			IsError      bool     `json:"is_error"`
			NumTurns     int      `json:"num_turns"`
			Errors       []string `json:"errors"`
			Message      *struct {
				Content []struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Content string `json:"content"`
				} `json:"content"`
			} `json:"message"`
			// Top-level content for tool_result events
			Content string `json:"content"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "result":
			if ev.TotalCostUSD > 0 {
				state.CostUSD = &ev.TotalCostUSD
			}
			if ev.DurationMS > 0 {
				state.DurationMS = &ev.DurationMS
			}
			if ev.Subtype != "" {
				resultSubtype = ev.Subtype
			}
			// Detect a crashed agent. A result line like
			// {"is_error":true,"subtype":"error_during_execution","num_turns":0,...}
			// means claude never did any work (e.g. a cross-worktree --resume
			// that couldn't find its conversation). Record the failure so
			// _finalize raises agent:needs-attention instead of falsely
			// reporting completion. A budget-cap result is handled separately
			// by the budget-pause heuristic and is not treated as a crash here.
			if ev.IsError || ev.Subtype == "error_during_execution" {
				reason := ev.Subtype
				if reason == "" {
					reason = "error"
				}
				if len(ev.Errors) > 0 && ev.Errors[0] != "" {
					reason += ": " + ev.Errors[0]
				}
				state.FailureReason = &reason
			} else {
				// A clean result clears any failure recorded by an earlier
				// (partial) result line.
				state.FailureReason = nil
			}
			// Record the Claude conversation UUID so a later budget-paused
			// resume can restore the trajectory and run claude --resume.
			if ev.SessionID != "" {
				sid := ev.SessionID
				state.ClaudeSessionID = &sid
			}
		case "assistant":
			if ev.Message != nil {
				for _, block := range ev.Message.Content {
					if block.Type == "text" {
						if url := extractPRURL(block.Text); url != "" {
							state.PRURL = &url
						}
					}
				}
			}
		default:
			// Handle tool_result and other event types that may
			// contain the PR URL (e.g. gh pr create output).
			if ev.Content != "" {
				if url := extractPRURL(ev.Content); url != "" {
					state.PRURL = &url
				}
			}
			if ev.Message != nil {
				for _, block := range ev.Message.Content {
					text := block.Text
					if text == "" {
						text = block.Content
					}
					if text != "" {
						if url := extractPRURL(text); url != "" {
							state.PRURL = &url
						}
					}
				}
			}
		}
	}

	if existingPRURL != "" {
		state.PRURL = &existingPRURL
	}

	return resultSubtype, store.Save(state)
}

// prURLExtractRegex matches GitHub PR URLs in free-form text, including
// inside markdown links, angle brackets, or adjacent punctuation.
var prURLExtractRegex = regexp.MustCompile(`https?://github\.com/[^\s"<>\]]+/pull/\d+`)

func extractPRURL(text string) string {
	return prURLExtractRegex.FindString(text)
}

var prURLRegex = regexp.MustCompile(`/pull/(\d+)`)

// extractPRNumberFromURL extracts the PR number from a GitHub PR URL.
// For example, "https://github.com/owner/repo/pull/123" returns "123".
func extractPRNumberFromURL(prURL string) string {
	matches := prURLRegex.FindStringSubmatch(prURL)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

// claudeSessionExists returns true if a Claude session JSONL file with the
// given UUID exists under ~/.claude/projects/. Claude stores each session at
// ~/.claude/projects/<encoded-project-dir>/<session-uuid>.jsonl; if the file
// has been cleaned up, "claude --resume <uuid>" exits at startup with 0 turns.
func claudeSessionExists(sessionUUID string) bool {
	if sessionUUID == "" {
		return false
	}
	// Validate the UUID before interpolating into a filepath.Glob pattern:
	// reject anything containing glob metacharacters or path separators so a
	// crafted log entry can't escape the projects directory or match
	// unintended files.
	for _, r := range sessionUUID {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	pattern := filepath.Join(home, ".claude", "projects", "*", sessionUUID+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return false
	}
	return len(matches) > 0
}

// ExtractClaudeSessionID parses a Claude stream-json JSONL log file and
// returns the session_id from the "result" event. Returns empty string if
// not found or on any error.
func ExtractClaudeSessionID(logPath string) string {
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	var ev struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Type == "result" && ev.SessionID != "" {
			return ev.SessionID
		}
	}
	return ""
}

func syncRunToDataRef(ctx context.Context, root string, store run.StateStore, gitClient git.Client, dataRef string, state *run.State) {
	stateFile := store.StateDir() + "/" + state.ID + ".json"
	files := map[string]string{
		"runs/" + state.ID + ".json": stateFile,
	}

	// Check log sensitivity before including
	if state.LogFile != nil {
		logF, err := os.Open(*state.LogFile)
		if err == nil {
			findings := scan.CheckSensitivity(logF)
			logF.Close()

			if len(findings) == 0 {
				files["logs/"+state.ID+".jsonl"] = *state.LogFile
			} else {
				fmt.Fprintf(os.Stderr, "warning: skipping log push for %s: potentially sensitive data detected\n", state.ID)
				for _, f := range findings {
					fmt.Fprintf(os.Stderr, "  - %s\n", f.Category)
				}
				fmt.Fprintf(os.Stderr, "  Use 'klaus push-log %s' to push manually.\n", state.ID)
			}
		}
	}

	// Also capture the resume-able Claude conversation file. This is distinct
	// from the stream-json log above: claude --resume reads this file (under
	// ~/.claude/projects/...), not the stdout stream we tee into logs/. Storing
	// it lets 'klaus launch --pr' continue a budget-paused conversation.
	if convPath := findResumeConversation(state); convPath != "" {
		if cf, err := os.Open(convPath); err == nil {
			findings := scan.CheckSensitivity(cf)
			cf.Close()
			if len(findings) == 0 {
				files["sessions/"+state.ID+".jsonl"] = convPath
			} else {
				fmt.Fprintf(os.Stderr, "warning: skipping conversation push for %s: potentially sensitive data detected\n", state.ID)
			}
		}
	}

	if err := gitClient.SyncToDataRef(ctx, root, dataRef, "Run "+state.ID, files); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync to data ref: %v\n", err)
		return
	}

	if err := gitClient.PushDataRef(ctx, root, dataRef); err != nil {
		// Silently ignore push failures (no remote, etc.)
	}
}

func init() {
	rootCmd.AddCommand(formatStreamCmd)
	rootCmd.AddCommand(finalizeCmd)
}

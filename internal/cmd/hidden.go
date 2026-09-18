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
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/draft"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/github"
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
	Short:  "Format backend JSONL stream from stdin",
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

		// No PR (or a crash): the worktree may hold the only copy of the work.
		var salvage *salvageResult
		if !paused && (state.PRURL == nil || *state.PRURL == "" || state.FailureReason != nil) {
			salvage = salvageUnfinishedWork(ctx, budgetPauseRunner, gitClient, state, cfg.DefaultBranch)
		}
		// Persist before the data-ref sync, which reads the state file.
		if err := store.Save(state); err != nil {
			slog.Warn("failed to save state before sync", "id", state.ID, "err", err)
		}

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
		// A crashed or salvaged run emits agent:needs-attention instead of
		// falsely reporting agent:completed / agent:pr-created.
		if baseDir != "" && !paused {
			emitFinalizeEvents(baseDir, state, salvage)
		}
		syncRunToDataRef(ctx, syncRoot, store, gitClient, cfg.DataRef, state)

		if salvage != nil && salvage.dirty {
			// WIP commit failed; removing the worktree would discard it.
			fmt.Fprintf(os.Stderr, "warning: keeping worktree %s: uncommitted changes could not be committed\n", state.Worktree)
		} else {
			cleanupWorktree(ctx, store, gitClient, state, salvage != nil)
		}

		// Kill the tmux pane — _finalize is the last command in the pipeline,
		// so this is safe. The pane would otherwise stay open indefinitely.
		killAgentPane(ctx, store, tmux.NewExecClient(), state)

		return nil
	},
}

// emitFinalizeEvents emits the terminal events for a finalized, non-paused
// run. A salvaged or crashed run (FailureReason set) emits
// agent:needs-attention and nothing else, so a pipeline never mistakes it for
// a completed, PR-creating run. A normal run emits agent:completed plus
// agent:pr-created when a PR URL is known.
func emitFinalizeEvents(baseDir string, state *run.State, salvage *salvageResult) {
	if state == nil || baseDir == "" {
		return
	}
	if salvage != nil {
		data := map[string]interface{}{
			"id":     state.ID,
			"branch": state.Branch,
			"pushed": salvage.pushed,
			"reason": salvage.reason,
		}
		if salvage.dirty {
			data["worktree"] = state.Worktree
		}
		emitEvent(baseDir, state.ID, event.AgentNeedsAttention, data)
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

// salvageResult describes unfinished work finalize preserved.
type salvageResult struct {
	reason string // no_pr, session_limit, or the crash's FailureReason
	pushed bool   // branch tip is on origin
	dirty  bool   // WIP commit failed; worktree still holds uncommitted changes
}

// salvageUnfinishedWork commits a dirty worktree as WIP and best-effort pushes
// the branch; cleanupWorktree then keeps any branch still unpushed. Returns nil
// when there is no work beyond the default branch. Never pushes onto a known
// PR, so WIP can't land on a live review.
func salvageUnfinishedWork(ctx context.Context, r draft.Runner, gc git.Client, state *run.State, defaultBranch string) *salvageResult {
	wt, branch := state.Worktree, state.Branch
	if wt == "" || branch == "" {
		return nil
	}
	if _, err := os.Stat(wt); err != nil {
		return nil
	}

	res := &salvageResult{reason: "no_pr"}
	if state.FailureReason != nil {
		res.reason = *state.FailureReason
	}
	if state.LogFile != nil && hitSessionLimit(*state.LogFile) {
		res.reason = "session_limit"
	}

	if _, err := draft.CommitWIP(ctx, r, wt, "wip: klaus finalize salvage "+state.ID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: salvage WIP commit: %v\n", err)
		res.dirty = true
	}
	// Unknown ahead count (error) is treated as work.
	if ahead, err := gc.CommitsAhead(ctx, wt, "origin/"+defaultBranch, branch); err == nil && ahead == 0 && !res.dirty {
		return nil
	}

	hasPR := state.PRURL != nil && *state.PRURL != ""
	if ok, err := gc.BranchPushed(ctx, wt, branch); err == nil && ok {
		res.pushed = true // origin tip == local tip, e.g. direct-push repos
	} else if !hasPR {
		if _, err := r.Git(ctx, wt, "push", "-u", "origin", branch); err != nil {
			fmt.Fprintf(os.Stderr, "warning: salvage push of %s: %v\n", branch, err)
		} else {
			res.pushed = true
		}
	}

	where := "local only"
	if res.pushed {
		where = "pushed"
	}
	note := fmt.Sprintf("%s; branch %s preserved (%s)", res.reason, branch, where)
	if res.dirty {
		note += "; uncommitted changes left in " + wt
	}
	state.NeedsAttention = &note
	return res
}

// sessionLimitRegex matches Claude usage-limit stops, e.g.
// "You've hit your session limit · resets 4:20pm".
var sessionLimitRegex = regexp.MustCompile(`(?i)hit your (session |weekly |usage )?limit|usage limit reached`)

// hitSessionLimit reports whether the log's last result event is a
// usage-limit stop (reported as subtype success, so no budget pause).
func hitSessionLimit(logPath string) bool {
	f, err := os.Open(logPath)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	hit := false
	for scanner.Scan() {
		var ev struct {
			Type   string `json:"type"`
			Result string `json:"result"`
		}
		if json.Unmarshal(stream.NormalizeLine(scanner.Bytes()), &ev) != nil || ev.Type != "result" {
			continue
		}
		hit = sessionLimitRegex.MatchString(ev.Result)
	}
	return hit
}

// cleanupWorktree removes the agent's worktree and local branch after
// completion. The state file and logs are preserved. The branch is kept when
// keepBranch is set or branchDeletable can't prove its work is safe. It is
// idempotent — if the worktree is already gone, the state is still cleared.
func cleanupWorktree(ctx context.Context, store run.StateStore, gitClient git.Client, state *run.State, keepBranch bool) {
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
	if state.Branch != "" && !keepBranch {
		if ok, err := branchDeletable(ctx, gitClient, gitRoot, state.Branch); !ok {
			slog.Warn("keeping branch: work not verified on origin", "id", state.ID, "branch", state.Branch, "err", err)
		} else if err := gitClient.BranchDelete(ctx, gitRoot, state.Branch); err != nil {
			slog.Warn("failed to delete branch during cleanup", "id", state.ID, "branch", state.Branch, "err", err)
		}
	}
	state.Worktree = ""
	if err := store.Save(state); err != nil {
		slog.Warn("failed to save state after worktree cleanup", "id", state.ID, "err", err)
	}
}

// branchDeletable: no commits beyond origin/<default>, or origin's live tip
// equals the local tip. Unverifiable (error) means keep.
func branchDeletable(ctx context.Context, gc git.Client, root, branch string) (bool, error) {
	cfg, _ := config.Load(root)
	if n, err := gc.CommitsAhead(ctx, root, "origin/"+cfg.DefaultBranch, branch); err == nil && n == 0 {
		return true, nil
	}
	return gc.BranchPushed(ctx, root, branch)
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
	sawResult := false
	var exactPRURL string
	var assistantPRURL string
	prTarget := prURLTargetSlug(state)
	for scanner.Scan() {
		line := stream.NormalizeLine(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var ev struct {
			Type         string   `json:"type"`
			ExitCode     int      `json:"exit_code"`
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

		if ev.SessionID != "" && !isClaudeRun(state) {
			state.BackendSessionID = stringPtr(ev.SessionID)
		}
		switch ev.Type {
		case "klaus_exit":
			// Claude structured results distinguish a limit stop from a crash.
			// Preserve that classification even when the process exits nonzero.
			if isClaudeRun(state) && sawResult {
				break
			}
			if ev.ExitCode != 0 {
				reason := fmt.Sprintf("%s exited with status %d", state.Backend, ev.ExitCode)
				state.FailureReason = &reason
				resultSubtype = "error_during_execution"
			}
		case "result":
			sawResult = true
			if ev.Content != "" {
				for _, url := range prURLExtractRegex.FindAllString(ev.Content, -1) {
					if isAllowedPRURL(prTarget, url) {
						exactPRURL = url
					}
				}
			}
			if ev.TotalCostUSD > 0 {
				state.CostUSD = &ev.TotalCostUSD
			}
			if ev.DurationMS > 0 {
				state.DurationMS = &ev.DurationMS
			}
			if ev.Subtype != "" {
				resultSubtype = ev.Subtype
			}
			// Claude session limits can report success with is_error set.
			limitStop := isClaudeRun(state) && (ev.Subtype == "error_max_budget_usd" || ev.Subtype == "error_max_turns")
			if ev.Subtype != "success" && !limitStop && (ev.IsError || ev.Subtype == "error_during_execution") {
				reason := ev.Subtype
				if reason == "" {
					reason = "error"
				}
				if len(ev.Errors) > 0 && ev.Errors[0] != "" {
					reason += ": " + ev.Errors[0]
				}
				state.FailureReason = &reason
			} else {
				// Clean results and limit stops clear earlier failures.
				state.FailureReason = nil
			}
			// Record the Claude conversation UUID so a later budget-paused
			// resume can restore the trajectory and run claude --resume.
			if ev.SessionID != "" && isClaudeRun(state) {
				sid := ev.SessionID
				state.ClaudeSessionID = &sid
			}
		case "assistant":
			if ev.Message != nil {
				for _, block := range ev.Message.Content {
					if block.Type == "text" {
						text := block.Text
						if text == "" {
							text = block.Content
						}
						for _, url := range prURLExtractRegex.FindAllString(text, -1) {
							if isAllowedPRURL(prTarget, url) {
								assistantPRURL = url
							}
						}
					}
				}
			}
		case "tool_result":
			if ev.Content != "" {
				for _, url := range prURLExtractRegex.FindAllString(ev.Content, -1) {
					if isAllowedPRURL(prTarget, url) {
						exactPRURL = url
					}
				}
			}
			fallthrough
		default:
			if ev.Message != nil {
				for _, block := range ev.Message.Content {
					if block.Type == "tool_result" || ev.Type == "tool_result" {
						text := block.Content
						if text == "" {
							text = block.Text
						}
						for _, url := range prURLExtractRegex.FindAllString(text, -1) {
							if isAllowedPRURL(prTarget, url) {
								exactPRURL = url
							}
						}
					}
				}
			}
		}
	}

	if existingPRURL != "" {
		state.PRURL = &existingPRURL
	} else if exactPRURL != "" {
		state.PRURL = &exactPRURL
	} else if assistantPRURL != "" {
		state.PRURL = &assistantPRURL
	}

	if err := scanner.Err(); err != nil {
		reason := fmt.Sprintf("reading backend log: %v", err)
		state.FailureReason = &reason
		resultSubtype = "error_during_execution"
	}
	if !isClaudeRun(state) {
		if !sawResult && state.FailureReason == nil {
			reason := "backend exited without a terminal result event"
			state.FailureReason = &reason
			resultSubtype = "error_during_execution"
		}
		elapsed := int64(1)
		if started, err := time.Parse(time.RFC3339, state.CreatedAt); err == nil {
			elapsed = max(int64(1), time.Since(started).Milliseconds())
		}
		if state.DurationMS == nil {
			state.DurationMS = &elapsed
		}
	}
	return resultSubtype, store.Save(state)
}

// prURLExtractRegex matches GitHub PR URLs in free-form text, including
// inside markdown links, angle brackets, or adjacent punctuation.
var prURLExtractRegex = regexp.MustCompile(`https?://github\.com/[^\s"<>\]]+/pull/\d+`)

func extractPRURL(text string) string {
	return prURLExtractRegex.FindString(text)
}

// prURLTargetSlug returns the "owner/repo" a run's PR URL must belong to: the
// run's TargetRepo when it is an owner/repo reference, otherwise the origin
// remote of the run's clone (or the current checkout). It returns "" when
// neither is a GitHub repo — e.g. TargetRepo is a project name or local path
// and origin is not a GitHub URL.
func prURLTargetSlug(state *run.State) string {
	if state != nil && state.TargetRepo != nil {
		if slug := repoSlug(*state.TargetRepo); slug != "" {
			return slug
		}
	}
	dir := ""
	if state != nil && state.CloneDir != nil {
		dir = *state.CloneDir
	} else {
		dir, _ = git.RepoRoot()
	}
	return repoSlugForDir(dir)
}

// isAllowedPRURL reports whether candidateURL may be recorded as a run's PR.
// Literal placeholder slugs (owner/repo and friends) are always rejected. When
// target (see prURLTargetSlug) is known the URL must belong to that repo; when
// it is unknown any other URL is accepted, so runs whose origin is not a
// GitHub URL still record their PR.
func isAllowedPRURL(target, candidateURL string) bool {
	slug := github.OwnerRepoFromPRURL(candidateURL)
	if slug == "" {
		return false
	}
	switch strings.ToLower(slug) {
	case "owner/repo", "<owner>/<repo>", "org/repo", "user/repo":
		return false
	}
	return target == "" || strings.EqualFold(slug, target)
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

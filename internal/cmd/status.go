package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show all runs and their current state",
	Long: `Shows all runs in the session and their current state.

STATUS is one of: running, needs-attention, pr-created, merged, closed,
no-access (GitHub won't show the PR with the current credentials), exited,
and, for runs that finished by pushing a branch without a PR:

  awaiting-integration  the branch is on origin and hasn't landed
  integrated            its work is on origin's default branch, by ancestry
                        or by an equal patch-id, or the valley says it landed
  branch-gone           the branch was deleted from origin without landing

and, for a branch finalize marked ready as a version of its change on a
valley, where the valley's record says the change stands:

  awaiting-approval     ready and waiting for you to approve it
  approved              approved; the integrator hasn't decided
  uncertain             a request was consumed or failed with no landing
                        evidence: look at it
  stale, rejected,      the integrator's decision on the request
  failed
  withdrawn             the change was withdrawn
  superseded            another version of the change is the active one
  draft                 returned to draft ('valley unready'), as launch
                        does when a follow-up run continues the change;
                        approve refuses it until a head is ready again

A run that isn't running and isn't done shows working while another run of
its change (the same change ID in the same repo) is running: a follow-up
(--pr, --resume-from or a pipeline fix) is still pushing to it, so there is
nothing to approve yet.

A valley run shows its change's handle in place of its run ID, and its
title as the prompt.

Integration is checked with one "valley changes --all --json" when a valley
is involved, and otherwise against each run's clone (one ls-remote and fetch
per repo). If the valley can't be asked, its runs are checked against their
clones. If the check can't complete, the run stays awaiting-integration (or
awaiting-approval). A deleted branch alone never settles a valley change: it
stays awaiting-approval rather than branch-gone.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		tmuxClient := tmux.NewExecClient()

		store, err := sessionStore()
		if err != nil {
			return err
		}
		states, err := store.List()
		if err != nil {
			return err
		}

		if len(states) == 0 {
			fmt.Println("No runs found.")
			return nil
		}

		ghClient := gh.NewGHCLIClient("")

		fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-8s  %-6s  %-20s  %-15s  %-6s  %-10s  %-10s  %-8s  %-10s  %s\n",
			"RUN ID", "STATUS", "BACKEND", "COST", "ISSUE", "REPO", "HOST", "PR", "CI", "CONFLICTS", "BEHIND", "MERGE", "PROMPT")
		fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-8s  %-6s  %-20s  %-15s  %-6s  %-10s  %-10s  %-8s  %-10s  %s\n",
			"------", "------", "-------", "----", "-----", "----", "----", "--", "--", "---------", "------", "-----", "------")

		statuses := make([]string, len(states))
		var pushed []*run.State
		for i, s := range states {
			if err := ctx.Err(); err != nil {
				return err
			}
			statuses[i] = determineStatus(ctx, s, tmuxClient)
			if statuses[i] == "awaiting-integration" {
				pushed = append(pushed, s)
			}
		}
		// Where pushed branches stand: one valley listing for the runs a
		// valley knows, one check per repo against each run's own clone for
		// the rest.
		results := integration.NewValleyChecker(nil).Check(ctx, pushed)

		// The running runs of each change: a run that isn't running or done
		// is working while another run of its change is running.
		running := make(map[string]bool, len(states))
		for i, s := range states {
			running[s.ID] = statuses[i] == "running"
		}
		working := runningByChange(states, func(s *run.State) bool { return running[s.ID] })

		for i, s := range states {
			if err := ctx.Err(); err != nil {
				return err
			}
			status := statuses[i]
			id := s.ID
			prompt := truncate(s.Prompt, 40)
			if status == "awaiting-integration" {
				res := results[s.ID]
				change := res.Change
				if change == nil {
					change = integration.RecordedChange(s)
				}
				status = pushedStatus(res.Verdict, change != nil)
				if change != nil {
					id = change.Handle
					if change.Title != "" {
						prompt = truncate(change.Title, 40)
					}
				}
			}
			cost := formatCost(s)
			issue := "-"
			if s.Issue != nil {
				issue = *s.Issue
			}
			repo := "-"
			if s.TargetRepo != nil {
				repo = truncate(*s.TargetRepo, 20)
			}
			host := "-"
			if s.Host != nil {
				host = *s.Host
			}
			ci, conflicts, behind, merge := "-", "-", "-", "-"
			if prRef := extractPRRef(s); prRef != "" {
				prState := ghClient.GetState(ctx, prRef)
				switch prState {
				case "MERGED":
					status = "merged"
					merge = "merged"
				case "CLOSED":
					status = "closed"
				case gh.StateNoAccess:
					// GitHub won't show this PR to us: nothing else to ask.
					status = "no-access"
				default:
					ci = ghClient.GetCI(ctx, prRef)
					conflicts = ghClient.GetConflicts(ctx, prRef)
					behindBy := ghClient.GetCommitsBehind(ctx, prRef)
					if behindBy > 0 {
						behind = fmt.Sprintf("%d", behindBy)
					}
					merge = computeMergeStatus(ci, conflicts, ghClient.GetReviewDecision(ctx, prRef), behindBy)
				}
			}

			if live(status) && len(workingOn([]*run.State{s}, working)) > 0 {
				status = "working"
			}

			fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-8s  %-6s  %-20s  %-15s  %-6s  %-10s  %-10s  %-8s  %-10s  %s\n",
				id, status, runBackendName(s), cost, issue, repo, host, formatPR(s), ci, conflicts, behind, merge, prompt)
		}

		return nil
	},
}

func determineStatus(ctx context.Context, s *run.State, tc tmux.Client) string {
	if s.Type == "session" {
		if _, err := os.Stat(s.Worktree); err == nil {
			return "active"
		}
		return "ended"
	}

	if s.Type == "track" {
		if s.MergedAt != nil {
			return "merged"
		}
		return "tracking"
	}

	if s.TmuxPane != nil && tc.PaneExists(ctx, *s.TmuxPane) {
		return "running"
	}

	if s.MergedAt != nil {
		return "merged"
	}
	if s.NeedsAttention != nil {
		return "needs-attention"
	}
	if s.PRURL != nil {
		return "pr-created"
	}
	if s.PushedSHA != nil {
		// Direct-push finish: the branch on origin waits for the operator.
		// The status command refines this with an integration check.
		return "awaiting-integration"
	}
	return "exited"
}

// live reports whether a status leaves something to do for a run that isn't
// running: salvaged work, an open PR, or a pushed branch that hasn't landed,
// gone, or been withdrawn or superseded.
func live(status string) bool {
	switch status {
	case "needs-attention", "pr-created", "awaiting-integration", "awaiting-approval",
		"approved", "uncertain", "stale", "rejected", "failed", "draft":
		return true
	}
	return false
}

// pushedStatus is the status of a run awaiting integration once an
// integration check has looked at it; valley says a valley knows the run's
// change. An unknown verdict (no clone, origin unreachable) keeps it
// awaiting.
func pushedStatus(v integration.Verdict, valley bool) string {
	switch v {
	case integration.Integrated, integration.IntegratedContent:
		return "integrated"
	case integration.Gone:
		return "branch-gone"
	case integration.Ready:
		return "awaiting-approval"
	case integration.Unknown, integration.Awaiting:
		if valley {
			return "awaiting-approval"
		}
		return "awaiting-integration"
	}
	// approved, uncertain, stale, rejected, failed, withdrawn, superseded,
	// draft
	return v.String()
}

func formatCost(s *run.State) string {
	if s.CostUSD != nil {
		return fmt.Sprintf("$%.2f", *s.CostUSD)
	}
	if s.Budget != nil {
		return fmt.Sprintf("<$%s", *s.Budget)
	}
	return "-"
}

func formatPR(s *run.State) string {
	if s.PRURL == nil {
		return "-"
	}
	url := *s.PRURL
	// Extract PR number from URL
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '/' {
			return "#" + url[i+1:]
		}
	}
	return url
}

// extractPRNumber returns the PR number (e.g. "18") from a run state, or "".
func extractPRNumber(s *run.State) string {
	if s.PRURL == nil {
		return ""
	}
	url := *s.PRURL
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '/' {
			return url[i+1:]
		}
	}
	return ""
}

// extractPRRef returns the full PR URL from a run state (for use with gh CLI),
// or "" if no PRURL is set. Using the full URL ensures gh can find the PR
// regardless of the current working directory.
func extractPRRef(s *run.State) string {
	if s.PRURL == nil {
		return ""
	}
	return *s.PRURL
}

// computeMergeStatus determines overall merge readiness. behind is the base-ahead
// commit count; a would-be-ready-but-behind PR reads "behind N" (rebased at merge)
// so it is never shown as a clean "ready".
func computeMergeStatus(ci, conflicts, reviewDecision string, behind int) string {
	if conflicts == "yes" {
		return "blocked"
	}
	if ci == "failing" {
		return "blocked"
	}
	if strings.EqualFold(reviewDecision, "CHANGES_REQUESTED") {
		return "blocked"
	}
	if ci == "pending" || reviewDecision == "unknown" {
		return "pending"
	}
	if ci == "passing" && conflicts == "none" && (strings.EqualFold(reviewDecision, "APPROVED") || reviewDecision == "") {
		if behind > 0 {
			return fmt.Sprintf("behind %d", behind)
		}
		return "ready"
	}
	return "pending"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runBackendName(s *run.State) string {
	if s.Type == "track" {
		return "-"
	}
	if s.Backend == "" {
		return "claude"
	}
	return s.Backend
}

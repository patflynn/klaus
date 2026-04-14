package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show all runs and their current state",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		tmuxClient := tmux.NewExecClient()

		states, _, err := listStatesFromEnvOrAll()
		if err != nil {
			return err
		}

		if len(states) == 0 {
			fmt.Println("No runs found.")
			return nil
		}

		ghClient := gh.NewGHCLIClient("")

		fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-6s  %-20s  %-15s  %-6s  %-10s  %-10s  %-10s  %s\n",
			"RUN ID", "STATUS", "COST", "ISSUE", "REPO", "HOST", "PR", "CI", "CONFLICTS", "MERGE", "PROMPT")
		fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-6s  %-20s  %-15s  %-6s  %-10s  %-10s  %-10s  %s\n",
			"------", "------", "----", "-----", "----", "----", "--", "--", "---------", "-----", "------")

		for _, s := range states {
			if err := ctx.Err(); err != nil {
				return err
			}
			status := determineStatus(ctx, s, tmuxClient)
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
			pr := formatPR(s)
			prompt := truncate(s.Prompt, 40)

			ci, conflicts, merge := "-", "-", "-"
			if prRef := extractPRRef(s); prRef != "" {
				prState := ghClient.GetState(ctx, prRef)
				switch prState {
				case "MERGED":
					status = "merged"
					merge = "merged"
				case "CLOSED":
					status = "closed"
				default:
					ci = ghClient.GetCI(ctx, prRef)
					conflicts = ghClient.GetConflicts(ctx, prRef)
					merge = computeMergeStatus(ci, conflicts, ghClient.GetReviewDecision(ctx, prRef))
				}
			}

			fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-6s  %-20s  %-15s  %-6s  %-10s  %-10s  %-10s  %s\n",
				s.ID, status, cost, issue, repo, host, pr, ci, conflicts, merge, prompt)
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
	if s.PRURL != nil {
		return "pr-created"
	}
	return "exited"
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

// computeMergeStatus determines overall merge readiness.
func computeMergeStatus(ci, conflicts, reviewDecision string) string {
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

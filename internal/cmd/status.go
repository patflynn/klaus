package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show all runs and their current state",
	RunE: func(cmd *cobra.Command, args []string) error {
		states, _, err := listStatesFromEnvOrAll()
		if err != nil {
			return err
		}

		if len(states) == 0 {
			fmt.Println("No runs found.")
			return nil
		}

		fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-6s  %-20s  %-6s  %-10s  %-10s  %-10s  %s\n",
			"RUN ID", "STATUS", "COST", "ISSUE", "REPO", "PR", "CI", "CONFLICTS", "MERGE", "PROMPT")
		fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-6s  %-20s  %-6s  %-10s  %-10s  %-10s  %s\n",
			"------", "------", "----", "-----", "----", "--", "--", "---------", "-----", "------")

		for _, s := range states {
			status := determineStatus(s)
			cost := formatCost(s)
			issue := "-"
			if s.Issue != nil {
				issue = *s.Issue
			}
			repo := "-"
			if s.TargetRepo != nil {
				repo = truncate(*s.TargetRepo, 20)
			}
			pr := formatPR(s)
			prompt := truncate(s.Prompt, 40)

			ci, conflicts, merge := "-", "-", "-"
			if prNum := extractPRNumber(s); prNum != "" {
				prState := getPRState(prNum)
				switch prState {
				case "MERGED":
					status = "merged"
					merge = "merged"
				case "CLOSED":
					status = "closed"
				default:
					ci = getPRCI(prNum)
					conflicts = getPRConflicts(prNum)
					merge = computeMergeStatus(ci, conflicts, getPRReviewDecision(prNum))
				}
			}

			fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-6s  %-20s  %-6s  %-10s  %-10s  %-10s  %s\n",
				s.ID, status, cost, issue, repo, pr, ci, conflicts, merge, prompt)
		}

		return nil
	},
}

func determineStatus(s *run.State) string {
	if s.Type == "session" {
		if _, err := os.Stat(s.Worktree); err == nil {
			return "active"
		}
		return "ended"
	}

	if s.TmuxPane != nil && tmux.PaneExists(*s.TmuxPane) {
		return "running"
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

// ghPRChecksArgs returns the arguments for "gh pr checks" with correct flag placement.
func ghPRChecksArgs(prNumber string) []string {
	return []string{"pr", "checks", "--", prNumber}
}

// getPRCI checks CI status by running "gh pr checks" and summarizing pass/fail/pending.
func getPRCI(prNumber string) string {
	cmd := exec.Command("gh", ghPRChecksArgs(prNumber)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	output := stdout.String()

	if err != nil && output == "" {
		return "unknown"
	}

	var passing, failing, pending int
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "pass") {
			passing++
		} else if strings.Contains(lower, "fail") {
			failing++
		} else {
			pending++
		}
	}

	if failing > 0 {
		return "failing"
	}
	if pending > 0 {
		return "pending"
	}
	if passing > 0 {
		return "passing"
	}
	return "unknown"
}

// ghPRConflictsArgs returns the arguments for "gh pr view" to check merge conflicts.
func ghPRConflictsArgs(prNumber string) []string {
	return []string{"pr", "view", "--json", "mergeable", "-q", ".mergeable", "--", prNumber}
}

// getPRConflicts checks if a PR has merge conflicts.
func getPRConflicts(prNumber string) string {
	cmd := exec.Command("gh", ghPRConflictsArgs(prNumber)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "unknown"
	}
	val := strings.TrimSpace(stdout.String())
	if strings.EqualFold(val, "CONFLICTING") {
		return "yes"
	}
	return "none"
}

// ghPRReviewDecisionArgs returns the arguments for "gh pr view" to fetch review decision.
func ghPRReviewDecisionArgs(prNumber string) []string {
	return []string{"pr", "view", "--json", "reviewDecision", "-q", ".reviewDecision", "--", prNumber}
}

// getPRReviewDecision fetches the review decision for a PR.
func getPRReviewDecision(prNumber string) string {
	cmd := exec.Command("gh", ghPRReviewDecisionArgs(prNumber)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "unknown"
	}
	return strings.TrimSpace(stdout.String())
}

// ghPRStateArgs returns the arguments for "gh pr view" to fetch PR state.
func ghPRStateArgs(prNumber string) []string {
	return []string{"pr", "view", "--json", "state", "-q", ".state", "--", prNumber}
}

// getPRState returns the PR state (e.g. "OPEN", "MERGED", "CLOSED") by calling gh.
func getPRState(prNumber string) string {
	cmd := exec.Command("gh", ghPRStateArgs(prNumber)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "UNKNOWN"
	}
	val := strings.TrimSpace(stdout.String())
	if val == "" {
		return "UNKNOWN"
	}
	return strings.ToUpper(val)
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

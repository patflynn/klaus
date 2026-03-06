package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show all runs and their current state",
	RunE: func(cmd *cobra.Command, args []string) error {
		commonDir, err := git.CommonDir()
		if err != nil {
			return fmt.Errorf("not inside a git repository")
		}

		states, err := run.List(commonDir)
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
				ci = getPRCI(prNum)
				conflicts = getPRConflicts(prNum)
				merge = computeMergeStatus(ci, conflicts, getPRReviewDecision(prNum))
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

// getPRCI checks CI status by running "gh pr checks" and summarizing pass/fail/pending.
func getPRCI(prNumber string) string {
	cmd := exec.Command("gh", "pr", "checks", prNumber)
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

// getPRConflicts checks if a PR has merge conflicts.
func getPRConflicts(prNumber string) string {
	cmd := exec.Command("gh", "pr", "view", prNumber, "--json", "mergeable", "-q", ".mergeable")
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

// getPRReviewDecision fetches the review decision for a PR.
func getPRReviewDecision(prNumber string) string {
	cmd := exec.Command("gh", "pr", "view", prNumber, "--json", "reviewDecision", "-q", ".reviewDecision")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}

// computeMergeStatus determines overall merge readiness.
func computeMergeStatus(ci, conflicts, reviewDecision string) string {
	if conflicts == "yes" {
		return "blocked"
	}
	if ci == "failing" {
		return "blocked"
	}
	if ci == "pending" {
		return "pending"
	}
	switch strings.ToUpper(reviewDecision) {
	case "CHANGES_REQUESTED":
		return "blocked"
	case "APPROVED":
		return "ready"
	}
	if ci == "passing" && conflicts == "none" {
		return "ready"
	}
	return "pending"
}

// getRepoOwnerAndName returns owner/repo by querying gh.
func getRepoOwnerAndName() (string, string, error) {
	cmd := exec.Command("gh", "repo", "view", "--json", "owner,name", "-q", ".owner.login + \"/\" + .name")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", "", err
	}
	parts := strings.SplitN(strings.TrimSpace(stdout.String()), "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected repo format")
	}
	return parts[0], parts[1], nil
}

// ghAPIGet runs gh api with the given path and returns parsed JSON.
func ghAPIGet(path string) ([]byte, error) {
	cmd := exec.Command("gh", "api", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// ghAPIPost runs gh api with POST method and field arguments.
func ghAPIPost(path string, fields map[string]string) error {
	args := []string{"api", path, "-X", "POST"}
	for k, v := range fields {
		args = append(args, "-f", k+"="+v)
	}
	cmd := exec.Command("gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api POST %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// prReviewComment represents a single PR review comment from the GitHub API.
type prReviewComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	Path string `json:"path"`
}

// fetchPRReviewComments fetches review comments for a PR.
func fetchPRReviewComments(owner, repo, prNumber string) ([]prReviewComment, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls/%s/comments", owner, repo, prNumber)
	data, err := ghAPIGet(path)
	if err != nil {
		return nil, err
	}
	var comments []prReviewComment
	if err := json.Unmarshal(data, &comments); err != nil {
		return nil, fmt.Errorf("parsing review comments: %w", err)
	}
	return comments, nil
}

// replyToReviewComment posts a reply to a specific PR review comment.
func replyToReviewComment(owner, repo, prNumber string, commentID int64, body string) error {
	path := fmt.Sprintf("repos/%s/%s/pulls/%s/comments/%d/replies", owner, repo, prNumber, commentID)
	return ghAPIPost(path, map[string]string{"body": body})
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

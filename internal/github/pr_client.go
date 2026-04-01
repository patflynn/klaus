package github

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// PRClient provides methods for interacting with GitHub PRs via the gh CLI.
// It holds repo context so that --repo is always passed automatically.
type PRClient struct {
	repo string // owner/repo
}

// NewPRClient creates a PRClient for the given owner/repo.
// If repo is empty, gh will infer the repo from the current git directory.
func NewPRClient(repo string) *PRClient {
	return &PRClient{repo: repo}
}

// Repo returns the configured owner/repo string.
func (c *PRClient) Repo() string {
	return c.repo
}

// ghArgs builds a base arg list with --repo injected when set.
func (c *PRClient) ghArgs(base []string, prRef string) []string {
	args := make([]string, len(base))
	copy(args, base)
	if c.repo != "" {
		args = append(args, "--repo", c.repo)
	}
	args = append(args, "--", prRef)
	return args
}

// GetCI checks CI status by running "gh pr checks" and summarizing pass/fail/pending.
func (c *PRClient) GetCI(prRef string) string {
	args := c.ghArgs([]string{"pr", "checks"}, prRef)
	cmd := exec.Command("gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
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

// GetConflicts checks if a PR has merge conflicts.
func (c *PRClient) GetConflicts(prRef string) string {
	args := c.ghArgs([]string{"pr", "view", "--json", "mergeable", "-q", ".mergeable"}, prRef)
	cmd := exec.Command("gh", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "unknown"
	}
	val := strings.TrimSpace(stdout.String())
	if strings.EqualFold(val, "CONFLICTING") {
		return "yes"
	}
	if strings.EqualFold(val, "MERGEABLE") {
		return "none"
	}
	return "unknown"
}

// GetReviewDecision fetches the review decision for a PR.
func (c *PRClient) GetReviewDecision(prRef string) string {
	args := c.ghArgs([]string{"pr", "view", "--json", "reviewDecision", "-q", ".reviewDecision"}, prRef)
	cmd := exec.Command("gh", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "unknown"
	}
	return strings.TrimSpace(stdout.String())
}

// GetState returns the PR state (e.g. "OPEN", "MERGED", "CLOSED").
func (c *PRClient) GetState(prRef string) string {
	args := c.ghArgs([]string{"pr", "view", "--json", "state", "-q", ".state"}, prRef)
	cmd := exec.Command("gh", args...)
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

// GetBranch returns the head branch name for a PR.
func (c *PRClient) GetBranch(prRef string) (string, error) {
	args := c.ghArgs([]string{"pr", "view", "--json", "headRefName", "-q", ".headRefName"}, prRef)
	cmd := exec.Command("gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh pr view: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	branch := strings.TrimSpace(stdout.String())
	if branch == "" {
		return "", fmt.Errorf("could not determine branch for PR %s", prRef)
	}
	return branch, nil
}

// GetURL returns the HTML URL for a PR.
func (c *PRClient) GetURL(prRef string) (string, error) {
	args := c.ghArgs([]string{"pr", "view", "--json", "url", "-q", ".url"}, prRef)
	cmd := exec.Command("gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh pr view: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// GetTitle fetches the title of a PR.
func (c *PRClient) GetTitle(prRef string) string {
	args := c.ghArgs([]string{"pr", "view", "--json", "title", "-q", ".title"}, prRef)
	cmd := exec.Command("gh", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "(unknown)"
	}
	title := strings.TrimSpace(stdout.String())
	if title == "" {
		return "(unknown)"
	}
	return title
}

// Merge merges a PR with the given method.
func (c *PRClient) Merge(prNumber, mergeMethod string, deleteBranch bool) error {
	args := MergeArgs(prNumber, mergeMethod, deleteBranch, c.repo)
	cmd := exec.Command("gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr merge: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// MergeArgs returns arguments for merging a PR. Exported for testing.
func MergeArgs(prNumber, mergeMethod string, deleteBranch bool, repo string) []string {
	args := []string{"pr", "merge"}
	switch mergeMethod {
	case "squash":
		args = append(args, "--squash")
	case "merge":
		args = append(args, "--merge")
	case "rebase":
		args = append(args, "--rebase")
	}
	if deleteBranch {
		args = append(args, "--delete-branch")
	}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	args = append(args, "--", prNumber)
	return args
}

// ChecksArgs returns arguments for "gh pr checks". Exported for testing.
func (c *PRClient) ChecksArgs(prRef string) []string {
	return c.ghArgs([]string{"pr", "checks"}, prRef)
}

// ViewStateArgs returns arguments for "gh pr view --json state". Exported for testing.
func (c *PRClient) ViewStateArgs(prRef string) []string {
	return c.ghArgs([]string{"pr", "view", "--json", "state", "-q", ".state"}, prRef)
}

// ViewConflictsArgs returns arguments for "gh pr view --json mergeable". Exported for testing.
func (c *PRClient) ViewConflictsArgs(prRef string) []string {
	return c.ghArgs([]string{"pr", "view", "--json", "mergeable", "-q", ".mergeable"}, prRef)
}

// ViewReviewDecisionArgs returns arguments for "gh pr view --json reviewDecision". Exported for testing.
func (c *PRClient) ViewReviewDecisionArgs(prRef string) []string {
	return c.ghArgs([]string{"pr", "view", "--json", "reviewDecision", "-q", ".reviewDecision"}, prRef)
}

// ViewTitleArgs returns arguments for "gh pr view --json title". Exported for testing.
func (c *PRClient) ViewTitleArgs(prRef string) []string {
	return c.ghArgs([]string{"pr", "view", "--json", "title", "-q", ".title"}, prRef)
}

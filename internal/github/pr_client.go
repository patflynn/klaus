package github

import (
	"strings"
)

// PRClient provides methods for interacting with GitHub PRs via the gh CLI.
// Deprecated: Use GHCLIClient (which implements the Client interface) instead.
// PRClient is kept for backward compatibility with existing test helpers.
type PRClient = GHCLIClient

// NewPRClient creates a PRClient for the given owner/repo.
// Deprecated: Use NewGHCLIClient instead.
func NewPRClient(repo string) *PRClient {
	return NewGHCLIClient(repo)
}

// ParseCIStatus categorizes "gh pr checks" output into passing/failing/pending/unknown.
// This is a standalone pure-parsing function, not tied to any client.
func ParseCIStatus(output string) string {
	var passing, failing, pending int
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "pass") || strings.Contains(lower, "skipping") {
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
	// No checks matched any category
	return "unknown"
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

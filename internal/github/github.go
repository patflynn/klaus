package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// GetRepoOwnerAndName returns owner/repo by querying gh.
func GetRepoOwnerAndName() (string, string, error) {
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

// APIGet runs gh api with the given path and returns parsed JSON.
func APIGet(path string) ([]byte, error) {
	cmd := exec.Command("gh", "api", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// APIPost runs gh api with POST method and field arguments.
func APIPost(path string, fields map[string]string) error {
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

// PRReviewComment represents a single PR review comment from the GitHub API.
type PRReviewComment struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	Path   string `json:"path"`
	User   commentUser `json:"user"`
}

type commentUser struct {
	Login string `json:"login"`
}

// FetchPRReviewComments fetches review comments for a PR.
func FetchPRReviewComments(owner, repo, prNumber string) ([]PRReviewComment, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls/%s/comments", owner, repo, prNumber)
	data, err := APIGet(path)
	if err != nil {
		return nil, err
	}
	var comments []PRReviewComment
	if err := json.Unmarshal(data, &comments); err != nil {
		return nil, fmt.Errorf("parsing review comments: %w", err)
	}
	return comments, nil
}

// ReplyToReviewComment posts a reply to a specific PR review comment.
func ReplyToReviewComment(owner, repo, prNumber string, commentID int64, body string) error {
	path := fmt.Sprintf("repos/%s/%s/pulls/%s/comments/%d/replies", owner, repo, prNumber, commentID)
	return APIPost(path, map[string]string{"body": body})
}

// ReviewThread represents a GitHub PR review thread with its GraphQL node ID.
type ReviewThread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
}

// FetchReviewThreads fetches review threads for a PR using the GraphQL API.
// Returns threads with their GraphQL node IDs and resolved status.
func FetchReviewThreads(owner, repo string, prNumber int) ([]ReviewThread, error) {
	query := fmt.Sprintf(`{ repository(owner:%q, name:%q) { pullRequest(number: %d) { reviewThreads(first: 100) { nodes { id isResolved } } } } }`, owner, repo, prNumber)
	return fetchReviewThreadsWithRunner(query, defaultGraphQLRunner)
}

// graphQLRunner abstracts the gh api graphql call for testing.
type graphQLRunner func(query string) ([]byte, error)

func defaultGraphQLRunner(query string) ([]byte, error) {
	cmd := exec.Command("gh", "api", "graphql", "-f", "query="+query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api graphql: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func fetchReviewThreadsWithRunner(query string, runner graphQLRunner) ([]ReviewThread, error) {
	data, err := runner(query)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []ReviewThread `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parsing review threads: %w", err)
	}
	return result.Data.Repository.PullRequest.ReviewThreads.Nodes, nil
}

// ResolveReviewThread resolves a review thread by its GraphQL node ID.
func ResolveReviewThread(threadID string) error {
	return resolveReviewThreadWithRunner(threadID, defaultGraphQLRunner)
}

func resolveReviewThreadWithRunner(threadID string, runner graphQLRunner) error {
	query := fmt.Sprintf(`mutation { resolveReviewThread(input: {threadId: %q}) { thread { isResolved } } }`, threadID)
	data, err := runner(query)
	if err != nil {
		return err
	}
	var result struct {
		Data struct {
			ResolveReviewThread struct {
				Thread struct {
					IsResolved bool `json:"isResolved"`
				} `json:"thread"`
			} `json:"resolveReviewThread"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parsing resolve response: %w", err)
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", result.Errors[0].Message)
	}
	return nil
}

// FetchCollaborators returns the list of collaborator logins for a repo.
func FetchCollaborators(owner, repo string) ([]string, error) {
	cmd := exec.Command("gh", "api", fmt.Sprintf("repos/%s/%s/collaborators", owner, repo), "--jq", ".[].login")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("fetching collaborators: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var logins []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line != "" {
			logins = append(logins, line)
		}
	}
	return logins, nil
}

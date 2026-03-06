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

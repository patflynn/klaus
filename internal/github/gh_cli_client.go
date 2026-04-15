package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Verify GHCLIClient implements Client at compile time.
var _ Client = (*GHCLIClient)(nil)

// defaultTimeout is the timeout for gh CLI calls (API operations).
const defaultTimeout = 30 * time.Second

// GHCLIClient implements Client by shelling out to the gh CLI.
type GHCLIClient struct {
	repo string // owner/repo, may be empty
}

// NewGHCLIClient creates a GHCLIClient. If repo is empty, gh will infer the
// repo from the current git directory for commands that need one.
func NewGHCLIClient(repo string) *GHCLIClient {
	return &GHCLIClient{repo: repo}
}

// Repo returns the configured owner/repo string.
func (c *GHCLIClient) Repo() string {
	return c.repo
}

// ghArgs builds a base arg list with --repo injected when set.
func (c *GHCLIClient) ghArgs(base []string, prRef string) []string {
	args := make([]string, len(base))
	copy(args, base)
	if c.repo != "" {
		args = append(args, "--repo", c.repo)
	}
	args = append(args, "--", prRef)
	return args
}

// ensureTimeout returns a context with defaultTimeout applied if the parent
// context has no deadline set. If ctx is nil, a background context is used.
func ensureTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}

// wrapTimeoutErr checks if an error is due to a context deadline and returns
// a clearer message.
func wrapTimeoutErr(ctx context.Context, cmdName string, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s timed out after %s", cmdName, defaultTimeout)
	}
	return err
}

// GetRepoOwnerAndName returns owner/repo by querying gh.
func (c *GHCLIClient) GetRepoOwnerAndName(ctx context.Context) (string, string, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "repo", "view", "--json", "owner,name", "-q", ".owner.login + \"/\" + .name")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", "", wrapTimeoutErr(ctx, "gh repo view", err)
	}
	parts := strings.SplitN(strings.TrimSpace(stdout.String()), "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected repo format")
	}
	return parts[0], parts[1], nil
}

// GetRepoOwnerAndNameFromDir returns owner/repo for the git repo at the given directory.
func (c *GHCLIClient) GetRepoOwnerAndNameFromDir(ctx context.Context, dir string) (string, string, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "repo", "view", "--json", "owner,name", "-q", ".owner.login + \"/\" + .name")
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("resolving repo for %s: %w", dir, wrapTimeoutErr(ctx, "gh repo view", err))
	}
	parts := strings.SplitN(strings.TrimSpace(stdout.String()), "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected repo format for %s", dir)
	}
	return parts[0], parts[1], nil
}

// GetCI checks CI status by running "gh pr checks" and summarizing pass/fail/pending.
func (c *GHCLIClient) GetCI(ctx context.Context, prRef string) string {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	args := c.ghArgs([]string{"pr", "checks"}, prRef)
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := stdout.String()

	if err != nil && output == "" {
		if strings.Contains(stderr.String(), "no checks reported") {
			return "passing" // no checks configured — nothing to fail
		}
		return "unknown"
	}

	if err == nil && strings.TrimSpace(output) == "" {
		return "unknown"
	}

	return ParseCIStatus(output)
}

// GetConflicts checks if a PR has merge conflicts.
func (c *GHCLIClient) GetConflicts(ctx context.Context, prRef string) string {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	args := c.ghArgs([]string{"pr", "view", "--json", "mergeable", "-q", ".mergeable"}, prRef)
	cmd := exec.CommandContext(ctx, "gh", args...)
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
func (c *GHCLIClient) GetReviewDecision(ctx context.Context, prRef string) string {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	args := c.ghArgs([]string{"pr", "view", "--json", "reviewDecision", "-q", ".reviewDecision"}, prRef)
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "unknown"
	}
	return strings.TrimSpace(stdout.String())
}

// GetState returns the PR state (e.g. "OPEN", "MERGED", "CLOSED").
func (c *GHCLIClient) GetState(ctx context.Context, prRef string) string {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	args := c.ghArgs([]string{"pr", "view", "--json", "state", "-q", ".state"}, prRef)
	cmd := exec.CommandContext(ctx, "gh", args...)
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
func (c *GHCLIClient) GetBranch(ctx context.Context, prRef string) (string, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	args := c.ghArgs([]string{"pr", "view", "--json", "headRefName", "-q", ".headRefName"}, prRef)
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh pr view: %w", wrapTimeoutErr(ctx, "gh pr view", err))
	}
	branch := strings.TrimSpace(stdout.String())
	if branch == "" {
		return "", fmt.Errorf("could not determine branch for PR %s", prRef)
	}
	return branch, nil
}

// GetURL returns the HTML URL for a PR.
func (c *GHCLIClient) GetURL(ctx context.Context, prRef string) (string, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	args := c.ghArgs([]string{"pr", "view", "--json", "url", "-q", ".url"}, prRef)
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh pr view: %w", wrapTimeoutErr(ctx, "gh pr view", err))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// GetTitle fetches the title of a PR.
func (c *GHCLIClient) GetTitle(ctx context.Context, prRef string) string {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	args := c.ghArgs([]string{"pr", "view", "--json", "title", "-q", ".title"}, prRef)
	cmd := exec.CommandContext(ctx, "gh", args...)
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
func (c *GHCLIClient) Merge(ctx context.Context, prNumber, mergeMethod string, deleteBranch bool) error {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	args := MergeArgs(prNumber, mergeMethod, deleteBranch, c.repo)
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr merge: %w", wrapTimeoutErr(ctx, "gh pr merge", err))
	}
	return nil
}

// FetchPRReviewComments fetches review comments for a PR.
func (c *GHCLIClient) FetchPRReviewComments(ctx context.Context, owner, repo, prNumber string) ([]PRReviewComment, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls/%s/comments", owner, repo, prNumber)
	data, err := c.APIGet(ctx, path)
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
func (c *GHCLIClient) ReplyToReviewComment(ctx context.Context, owner, repo, prNumber string, commentID int64, body string) error {
	path := fmt.Sprintf("repos/%s/%s/pulls/%s/comments/%d/replies", owner, repo, prNumber, commentID)
	return c.APIPost(ctx, path, map[string]string{"body": body})
}

// FetchReviewThreads fetches review threads for a PR using the GraphQL API.
func (c *GHCLIClient) FetchReviewThreads(ctx context.Context, owner, repo string, prNumber int) ([]ReviewThread, error) {
	query := fmt.Sprintf(`{ repository(owner:%q, name:%q) { pullRequest(number: %d) { reviewThreads(first: 100) { nodes { id isResolved } } } } }`, owner, repo, prNumber)
	return fetchReviewThreadsImpl(query, func(q string) ([]byte, error) {
		return c.runGraphQL(ctx, q)
	})
}

// ResolveReviewThread resolves a review thread by its GraphQL node ID.
func (c *GHCLIClient) ResolveReviewThread(ctx context.Context, threadID string) error {
	return resolveReviewThreadImpl(threadID, func(q string) ([]byte, error) {
		return c.runGraphQL(ctx, q)
	})
}

// runGraphQL executes a GraphQL query via gh api.
func (c *GHCLIClient) runGraphQL(ctx context.Context, query string) ([]byte, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api graphql: %w", wrapTimeoutErr(ctx, "gh api graphql", err))
	}
	return stdout.Bytes(), nil
}

// fetchReviewThreadsImpl parses a GraphQL response for review threads.
func fetchReviewThreadsImpl(query string, runner graphQLRunner) ([]ReviewThread, error) {
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

// resolveReviewThreadImpl performs a resolve mutation and parses the response.
func resolveReviewThreadImpl(threadID string, runner graphQLRunner) error {
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
func (c *GHCLIClient) FetchCollaborators(ctx context.Context, owner, repo string) ([]string, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "api", fmt.Sprintf("repos/%s/%s/collaborators", owner, repo), "--jq", ".[].login")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("fetching collaborators: %w", wrapTimeoutErr(ctx, "gh api collaborators", err))
	}
	var logins []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line != "" {
			logins = append(logins, line)
		}
	}
	return logins, nil
}

// APIGet runs gh api with the given path and returns raw bytes.
func (c *GHCLIClient) APIGet(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "api", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api %s: %w", path, wrapTimeoutErr(ctx, "gh api "+path, err))
	}
	return stdout.Bytes(), nil
}

// APIPost runs gh api with POST method and field arguments.
func (c *GHCLIClient) APIPost(ctx context.Context, path string, fields map[string]string) error {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	args := []string{"api", path, "-X", "POST"}
	for k, v := range fields {
		args = append(args, "-f", k+"="+v)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api POST %s: %w", path, wrapTimeoutErr(ctx, "gh api POST "+path, err))
	}
	return nil
}

// APIPostJSON runs gh api with POST method and a raw JSON body via --input.
func (c *GHCLIClient) APIPostJSON(ctx context.Context, path string, body interface{}) ([]byte, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling request body: %w", err)
	}
	cmd := exec.CommandContext(ctx, "gh", "api", path, "-X", "POST", "--input", "-")
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api POST %s: %w", path, wrapTimeoutErr(ctx, "gh api POST "+path, err))
	}
	return stdout.Bytes(), nil
}

// FetchPRMetadata fetches PR URL, title, head branch, and state from GitHub via gh CLI.
func (c *GHCLIClient) FetchPRMetadata(ctx context.Context, prNumber, repo string) (prURL, title, headBranch, state string, err error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	args := []string{
		"pr", "view", prNumber,
		"--repo", repo,
		"--json", "url,title,headRefName,state",
		"-q", `(.url) + "\t" + (.title) + "\t" + (.headRefName) + "\t" + (.state)`,
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", "", "", wrapTimeoutErr(ctx, "gh pr view", fmt.Errorf("gh pr view: %s", strings.TrimSpace(stderr.String())))
	}

	parts := strings.SplitN(strings.TrimSpace(stdout.String()), "\t", 4)
	if len(parts) < 4 {
		return "", "", "", "", fmt.Errorf("unexpected gh output: %s", stdout.String())
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}

// GetAuthenticatedUser returns the login of the currently authenticated GitHub user.
func (c *GHCLIClient) GetAuthenticatedUser(ctx context.Context) (string, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "api", "user", "-q", ".login")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh api user: %w", wrapTimeoutErr(ctx, "gh api user", err))
	}
	login := strings.TrimSpace(stdout.String())
	if login == "" {
		return "", fmt.Errorf("empty login from gh api user")
	}
	return login, nil
}

// ChecksArgs returns arguments for "gh pr checks". Exported for testing.
func (c *GHCLIClient) ChecksArgs(prRef string) []string {
	return c.ghArgs([]string{"pr", "checks"}, prRef)
}

// ViewStateArgs returns arguments for "gh pr view --json state". Exported for testing.
func (c *GHCLIClient) ViewStateArgs(prRef string) []string {
	return c.ghArgs([]string{"pr", "view", "--json", "state", "-q", ".state"}, prRef)
}

// ViewConflictsArgs returns arguments for "gh pr view --json mergeable". Exported for testing.
func (c *GHCLIClient) ViewConflictsArgs(prRef string) []string {
	return c.ghArgs([]string{"pr", "view", "--json", "mergeable", "-q", ".mergeable"}, prRef)
}

// ViewReviewDecisionArgs returns arguments for "gh pr view --json reviewDecision". Exported for testing.
func (c *GHCLIClient) ViewReviewDecisionArgs(prRef string) []string {
	return c.ghArgs([]string{"pr", "view", "--json", "reviewDecision", "-q", ".reviewDecision"}, prRef)
}

// ViewTitleArgs returns arguments for "gh pr view --json title". Exported for testing.
func (c *GHCLIClient) ViewTitleArgs(prRef string) []string {
	return c.ghArgs([]string{"pr", "view", "--json", "title", "-q", ".title"}, prRef)
}

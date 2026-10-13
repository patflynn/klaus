package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// ReviewerResolver picks the GitHub user a launched agent requests PR review
// from. Agents open PRs as the gh-authenticated user, and GitHub silently drops
// a review request aimed at a PR's author, so the reviewer has to be someone
// else who can approve.
type ReviewerResolver struct {
	// Run runs gh with args and returns its stdout. Tests replace it.
	Run func(ctx context.Context, args ...string) ([]byte, error)

	authorOnce sync.Once
	author     string
	authorErr  error
}

// NewReviewerResolver returns a ReviewerResolver that runs the real gh CLI.
func NewReviewerResolver() *ReviewerResolver {
	return &ReviewerResolver{Run: runGH}
}

func runGH(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		err = wrapTimeoutErr(ctx, "gh "+strings.Join(args, " "), err)
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// Resolve returns the reviewer for PRs on repo ("owner/name"). A configured
// login is returned as given, without running gh. Otherwise the reviewer is
// chosen from the repo's collaborators with push access, leaving out the
// gh-authenticated user (the PR's author) and bots: the repo owner if they are
// one, else the only one. When there is no reviewer, warning says why in one
// line; a failed gh call ends up there too, never as an error.
func (r *ReviewerResolver) Resolve(ctx context.Context, repo, configured string) (reviewer, warning string) {
	if configured != "" {
		return configured, ""
	}
	reviewer, err := r.fromCollaborators(ctx, repo)
	if err != nil {
		return "", "no PR reviewer: " + oneLine(err) + "; set pr_reviewer to name one"
	}
	return reviewer, ""
}

func (r *ReviewerResolver) fromCollaborators(ctx context.Context, repo string) (string, error) {
	owner, _, ok := strings.Cut(repo, "/")
	if !ok || owner == "" {
		return "", fmt.Errorf("cannot tell the GitHub repo from origin")
	}
	author, err := r.authenticatedUser(ctx)
	if err != nil {
		return "", fmt.Errorf("looking up the gh user: %w", err)
	}
	out, err := r.Run(ctx, "api", "--paginate", "repos/"+repo+"/collaborators?permission=push&affiliation=all")
	if err != nil {
		return "", fmt.Errorf("listing collaborators of %s: %w", repo, err)
	}
	collaborators, err := parseCollaborators(out)
	if err != nil {
		return "", fmt.Errorf("reading collaborators of %s: %w", repo, err)
	}

	var candidates []string
	for _, c := range collaborators {
		if c.Type != "User" || strings.EqualFold(c.Login, author) {
			continue
		}
		if strings.EqualFold(c.Login, owner) {
			return c.Login, nil
		}
		candidates = append(candidates, c.Login)
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", fmt.Errorf("no user but %s has push access to %s", author, repo)
	default:
		return "", fmt.Errorf("%s has several possible reviewers (%s)", repo, strings.Join(candidates, ", "))
	}
}

// authenticatedUser returns the gh-authenticated login, looked up once per
// resolver.
func (r *ReviewerResolver) authenticatedUser(ctx context.Context) (string, error) {
	r.authorOnce.Do(func() {
		out, err := r.Run(ctx, "api", "user", "-q", ".login")
		if err != nil {
			r.authorErr = err
			return
		}
		r.author = strings.TrimSpace(string(out))
		if r.author == "" {
			r.authorErr = errors.New("gh api user returned an empty login")
		}
	})
	return r.author, r.authorErr
}

type collaborator struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// parseCollaborators reads `gh api --paginate` output, which is one JSON array
// per page written back to back.
func parseCollaborators(out []byte) ([]collaborator, error) {
	var all []collaborator
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var page []collaborator
		if err := dec.Decode(&page); errors.Is(err, io.EOF) {
			return all, nil
		} else if err != nil {
			return nil, err
		}
		all = append(all, page...)
	}
}

// oneLine collapses err's message onto a single line, since gh can print
// multi-line errors.
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

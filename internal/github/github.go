package github

import (
	"context"
)

// Package-level functions are kept for backward compatibility.
// They delegate to a default GHCLIClient.

// GetRepoOwnerAndName returns owner/repo by querying gh.
func GetRepoOwnerAndName() (string, string, error) {
	return NewGHCLIClient("").GetRepoOwnerAndName(context.TODO())
}

// APIGet runs gh api with the given path and returns parsed JSON.
func APIGet(path string) ([]byte, error) {
	return NewGHCLIClient("").APIGet(context.TODO(), path)
}

// APIPost runs gh api with POST method and field arguments.
func APIPost(path string, fields map[string]string) error {
	return NewGHCLIClient("").APIPost(context.TODO(), path, fields)
}

// APIPostJSON runs gh api with POST method and a raw JSON body via --input.
func APIPostJSON(path string, body interface{}) ([]byte, error) {
	return NewGHCLIClient("").APIPostJSON(context.TODO(), path, body)
}

// GetRepoOwnerAndNameFromDir returns owner/repo for the git repo at the given directory.
func GetRepoOwnerAndNameFromDir(dir string) (string, string, error) {
	return NewGHCLIClient("").GetRepoOwnerAndNameFromDir(context.TODO(), dir)
}

// PRReviewComment represents a single PR review comment from the GitHub API.
type PRReviewComment struct {
	ID   int64       `json:"id"`
	Body string      `json:"body"`
	Path string      `json:"path"`
	User commentUser `json:"user"`
}

type commentUser struct {
	Login string `json:"login"`
}

// FetchPRReviewComments fetches review comments for a PR.
func FetchPRReviewComments(owner, repo, prNumber string) ([]PRReviewComment, error) {
	return NewGHCLIClient("").FetchPRReviewComments(context.TODO(), owner, repo, prNumber)
}

// ReplyToReviewComment posts a reply to a specific PR review comment.
func ReplyToReviewComment(owner, repo, prNumber string, commentID int64, body string) error {
	return NewGHCLIClient("").ReplyToReviewComment(context.TODO(), owner, repo, prNumber, commentID, body)
}

// ReviewThread represents a GitHub PR review thread with its GraphQL node ID.
type ReviewThread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
}

// FetchReviewThreads fetches review threads for a PR using the GraphQL API.
func FetchReviewThreads(owner, repo string, prNumber int) ([]ReviewThread, error) {
	return NewGHCLIClient("").FetchReviewThreads(context.TODO(), owner, repo, prNumber)
}

// graphQLRunner abstracts the gh api graphql call for testing.
type graphQLRunner func(query string) ([]byte, error)

func defaultGraphQLRunner(query string) ([]byte, error) {
	return NewGHCLIClient("").runGraphQL(context.TODO(), query)
}

func fetchReviewThreadsWithRunner(query string, runner graphQLRunner) ([]ReviewThread, error) {
	return fetchReviewThreadsImpl(query, runner)
}

func resolveReviewThreadWithRunner(threadID string, runner graphQLRunner) error {
	return resolveReviewThreadImpl(threadID, runner)
}

// ResolveReviewThread resolves a review thread by its GraphQL node ID.
func ResolveReviewThread(threadID string) error {
	return NewGHCLIClient("").ResolveReviewThread(context.TODO(), threadID)
}

// FetchCollaborators returns the list of collaborator logins for a repo.
func FetchCollaborators(owner, repo string) ([]string, error) {
	return NewGHCLIClient("").FetchCollaborators(context.TODO(), owner, repo)
}

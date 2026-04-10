package github

import "context"

// Client defines all GitHub operations Klaus uses.
type Client interface {
	// Repository info
	GetRepoOwnerAndName(ctx context.Context) (owner string, repo string, err error)
	GetRepoOwnerAndNameFromDir(ctx context.Context, dir string) (owner string, repo string, err error)

	// PR queries
	GetCI(ctx context.Context, prRef string) string
	GetConflicts(ctx context.Context, prRef string) string
	GetReviewDecision(ctx context.Context, prRef string) string
	GetState(ctx context.Context, prRef string) string
	GetBranch(ctx context.Context, prRef string) (string, error)
	GetURL(ctx context.Context, prRef string) (string, error)
	GetTitle(ctx context.Context, prRef string) string

	// PR mutations
	Merge(ctx context.Context, prNumber, mergeMethod string, deleteBranch bool) error

	// Review operations
	FetchPRReviewComments(ctx context.Context, owner, repo, prNumber string) ([]PRReviewComment, error)
	ReplyToReviewComment(ctx context.Context, owner, repo, prNumber string, commentID int64, body string) error
	FetchReviewThreads(ctx context.Context, owner, repo string, prNumber int) ([]ReviewThread, error)
	ResolveReviewThread(ctx context.Context, threadID string) error

	// Collaborators
	FetchCollaborators(ctx context.Context, owner, repo string) ([]string, error)

	// Generic API
	APIGet(ctx context.Context, path string) ([]byte, error)
	APIPost(ctx context.Context, path string, fields map[string]string) error
	APIPostJSON(ctx context.Context, path string, body interface{}) ([]byte, error)

	// PR metadata (multi-field fetch for track command)
	FetchPRMetadata(ctx context.Context, prNumber, repo string) (prURL, title, headBranch, state string, err error)

	// Repo operations
	GetAuthenticatedUser(ctx context.Context) (login string, err error)
}

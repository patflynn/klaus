package git

import "context"

// Client defines the interface for all git operations Klaus uses.
// Implementations must be safe for concurrent use.
type Client interface {
	// CommonDir returns the absolute path to the git common directory.
	// This works from worktrees too (returns the main repo's .git dir).
	CommonDir(ctx context.Context) (string, error)

	// FetchAll fetches all branches and tags from origin, pruning deleted remote branches.
	FetchAll(ctx context.Context, repoDir string) error

	// FetchBranch fetches a single branch from origin.
	FetchBranch(ctx context.Context, repoDir, branch string) error

	// WorktreeAdd creates a new worktree at path on a new branch based on startPoint.
	// If the branch is already checked out in a stale worktree, it prunes and retries once.
	WorktreeAdd(ctx context.Context, repoDir, path, branch, startPoint string) error

	// WorktreeRemove removes a worktree. repoDir is the main repo directory.
	WorktreeRemove(ctx context.Context, repoDir, path string) error

	// WorktreeAddTrack creates a worktree tracking an existing remote branch.
	// If the branch is already checked out in a stale worktree, it prunes and retries once.
	WorktreeAddTrack(ctx context.Context, repoDir, path, branch string) error

	// WorktreePrune removes stale worktree tracking information.
	WorktreePrune(ctx context.Context, repoDir string) error

	// BranchDelete deletes a local branch.
	BranchDelete(ctx context.Context, repoDir, branch string) error

	// EnsureDataRef ensures the custom data ref exists. Creates it with an empty
	// initial commit if it doesn't.
	EnsureDataRef(ctx context.Context, repoDir, dataRef string) error

	// SyncToDataRef commits files to the data ref using a temporary index.
	// files is a map of path-in-tree -> local-file-path.
	SyncToDataRef(ctx context.Context, repoDir, dataRef, commitMsg string, files map[string]string) error

	// EnsureClone clones a repo to destDir if it doesn't already exist.
	// If it already exists, fetches the latest from origin.
	EnsureClone(ctx context.Context, cloneURL, destDir string) error

	// PushDataRef pushes the data ref to origin.
	PushDataRef(ctx context.Context, repoDir, dataRef string) error

	// InstallCommitMsgHook installs a commit-msg hook in the given worktree that
	// strips Claude/Anthropic attribution from commit messages.
	InstallCommitMsgHook(ctx context.Context, worktreeDir string) error
}

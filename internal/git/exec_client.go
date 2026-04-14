package git

import "context"

// ExecClient implements Client by shelling out to the git binary.
type ExecClient struct{}

// NewExecClient returns a new ExecClient.
func NewExecClient() *ExecClient {
	return &ExecClient{}
}

func (c *ExecClient) CommonDir(ctx context.Context) (string, error) {
	return CommonDir(ctx)
}

func (c *ExecClient) FetchAll(ctx context.Context, repoDir string) error {
	return FetchAll(ctx, repoDir)
}

func (c *ExecClient) FetchBranch(ctx context.Context, repoDir, branch string) error {
	return FetchBranch(ctx, repoDir, branch)
}

func (c *ExecClient) WorktreeAdd(ctx context.Context, repoDir, path, branch, startPoint string) error {
	return WorktreeAdd(ctx, repoDir, path, branch, startPoint)
}

func (c *ExecClient) WorktreeRemove(ctx context.Context, repoDir, path string) error {
	return WorktreeRemove(ctx, repoDir, path)
}

func (c *ExecClient) WorktreeAddTrack(ctx context.Context, repoDir, path, branch string) error {
	return WorktreeAddTrack(ctx, repoDir, path, branch)
}

func (c *ExecClient) WorktreePrune(ctx context.Context, repoDir string) error {
	return WorktreePrune(ctx, repoDir)
}

func (c *ExecClient) BranchDelete(ctx context.Context, repoDir, branch string) error {
	return BranchDelete(ctx, repoDir, branch)
}

func (c *ExecClient) EnsureDataRef(ctx context.Context, repoDir, dataRef string) error {
	return EnsureDataRef(ctx, repoDir, dataRef)
}

func (c *ExecClient) SyncToDataRef(ctx context.Context, repoDir, dataRef, commitMsg string, files map[string]string) error {
	return SyncToDataRef(ctx, repoDir, dataRef, commitMsg, files)
}

func (c *ExecClient) EnsureClone(ctx context.Context, cloneURL, destDir string) error {
	return EnsureClone(ctx, cloneURL, destDir)
}

func (c *ExecClient) PushDataRef(ctx context.Context, repoDir, dataRef string) error {
	return PushDataRef(ctx, repoDir, dataRef)
}

func (c *ExecClient) InstallCommitMsgHook(ctx context.Context, worktreeDir string) error {
	return InstallCommitMsgHook(ctx, worktreeDir)
}

// compile-time check
var _ Client = (*ExecClient)(nil)

package git

import "context"

// ExecClient implements Client by shelling out to the git binary.
type ExecClient struct{}

// NewExecClient returns a new ExecClient.
func NewExecClient() *ExecClient {
	return &ExecClient{}
}

func (c *ExecClient) CommonDir(_ context.Context) (string, error) {
	return CommonDir()
}

func (c *ExecClient) FetchAll(_ context.Context, repoDir string) error {
	return FetchAll(repoDir)
}

func (c *ExecClient) FetchBranch(_ context.Context, repoDir, branch string) error {
	return FetchBranch(repoDir, branch)
}

func (c *ExecClient) WorktreeAdd(_ context.Context, repoDir, path, branch, startPoint string) error {
	return WorktreeAdd(repoDir, path, branch, startPoint)
}

func (c *ExecClient) WorktreeRemove(_ context.Context, repoDir, path string) error {
	return WorktreeRemove(repoDir, path)
}

func (c *ExecClient) WorktreeAddTrack(_ context.Context, repoDir, path, branch string) error {
	return WorktreeAddTrack(repoDir, path, branch)
}

func (c *ExecClient) WorktreePrune(_ context.Context, repoDir string) error {
	return WorktreePrune(repoDir)
}

func (c *ExecClient) BranchDelete(_ context.Context, repoDir, branch string) error {
	return BranchDelete(repoDir, branch)
}

func (c *ExecClient) EnsureDataRef(_ context.Context, repoDir, dataRef string) error {
	return EnsureDataRef(repoDir, dataRef)
}

func (c *ExecClient) SyncToDataRef(_ context.Context, repoDir, dataRef, commitMsg string, files map[string]string) error {
	return SyncToDataRef(repoDir, dataRef, commitMsg, files)
}

func (c *ExecClient) EnsureClone(_ context.Context, cloneURL, destDir string) error {
	return EnsureClone(cloneURL, destDir)
}

func (c *ExecClient) PushDataRef(_ context.Context, repoDir, dataRef string) error {
	return PushDataRef(repoDir, dataRef)
}

func (c *ExecClient) InstallCommitMsgHook(_ context.Context, worktreeDir string) error {
	return InstallCommitMsgHook(worktreeDir)
}

// compile-time check
var _ Client = (*ExecClient)(nil)

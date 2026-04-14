package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Timeout constants for git operations.
const (
	// networkTimeout is for git commands that talk to a remote (fetch, push, clone).
	networkTimeout = 2 * time.Minute
	// localTimeout is for git commands that operate on local data (worktree, branch, rev-parse).
	localTimeout = 30 * time.Second
)

// RepoRoot returns the top-level directory of the git repository.
func RepoRoot() (string, error) {
	return runGit(context.Background(), "", "rev-parse", "--show-toplevel")
}

// CommonDir returns the absolute path to the git common directory.
// This works from worktrees too (returns the main repo's .git dir).
func CommonDir(ctx context.Context) (string, error) {
	d, err := runGit(ctx, "", "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	// --git-common-dir can return a relative path in the main worktree
	abs, err := filepath.Abs(d)
	if err != nil {
		return "", fmt.Errorf("resolving common dir: %w", err)
	}
	return abs, nil
}

// FetchAll fetches all branches and tags from origin, pruning deleted remote branches.
func FetchAll(ctx context.Context, repoDir string) error {
	_, err := runGitNetwork(ctx, repoDir, "fetch", "origin", "--prune", "--tags", "--quiet")
	return err
}

// FetchBranch fetches a branch from origin.
func FetchBranch(ctx context.Context, repoDir, branch string) error {
	_, err := runGitNetwork(ctx, repoDir, "fetch", "origin", branch, "--quiet")
	return err
}

// WorktreeAdd creates a new worktree at path on a new branch based on startPoint.
// If the branch is already checked out in a stale worktree, it prunes and retries once.
func WorktreeAdd(ctx context.Context, repoDir, path, branch, startPoint string) error {
	_, err := runGit(ctx, repoDir, "worktree", "add", path, "-b", branch, startPoint, "--quiet")
	if err == nil {
		return nil
	}
	return retryAfterPrune(ctx, repoDir, branch, err, func() error {
		// Use -B on retry: after pruning a stale worktree the branch ref remains,
		// so -b (create new) would fail with "branch already exists".
		_, e := runGit(ctx, repoDir, "worktree", "add", path, "-B", branch, startPoint, "--quiet")
		return e
	})
}

// WorktreeRemove removes a worktree. repoDir is the main repo directory.
func WorktreeRemove(ctx context.Context, repoDir, path string) error {
	_, err := runGit(ctx, repoDir, "worktree", "remove", "--force", path)
	return err
}

// WorktreeAddTrack creates a worktree tracking an existing remote branch.
// It uses -B to force-create/reset the local branch to match origin/<branch>.
// If the branch is already checked out in a stale worktree, it prunes and retries once.
func WorktreeAddTrack(ctx context.Context, repoDir, path, branch string) error {
	_, err := runGit(ctx, repoDir, "worktree", "add", "-B", branch, path, "origin/"+branch, "--quiet")
	if err == nil {
		return nil
	}
	return retryAfterPrune(ctx, repoDir, branch, err, func() error {
		_, e := runGit(ctx, repoDir, "worktree", "add", "-B", branch, path, "origin/"+branch, "--quiet")
		return e
	})
}

// WorktreePrune removes stale worktree tracking information.
func WorktreePrune(ctx context.Context, repoDir string) error {
	_, err := runGit(ctx, repoDir, "worktree", "prune")
	return err
}

// worktreePathForBranch returns the worktree path that has the given branch
// checked out, or "" if not found.
func worktreePathForBranch(ctx context.Context, repoDir, branch string) string {
	out, err := runGit(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	var currentPath string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "worktree ") {
			currentPath = strings.TrimPrefix(line, "worktree ")
		}
		if strings.HasPrefix(line, "branch ") {
			ref := strings.TrimPrefix(line, "branch ")
			// branch ref looks like "refs/heads/<branch>"
			if ref == "refs/heads/"+branch {
				return currentPath
			}
		}
	}
	return ""
}

// retryAfterPrune handles "already used by worktree" errors. If the conflicting
// worktree is stale (path no longer exists on disk), it prunes and retries.
// If the worktree is still live, it returns a clear error.
func retryAfterPrune(ctx context.Context, repoDir, branch string, origErr error, retry func() error) error {
	// Check whether this branch is recorded in any worktree.
	wtPath := worktreePathForBranch(ctx, repoDir, branch)
	if wtPath == "" {
		// Branch is not in any worktree — nothing to prune; return the original error.
		return origErr
	}

	// If the worktree directory still exists, the worktree is live — don't touch it.
	if _, err := os.Stat(wtPath); err == nil {
		return fmt.Errorf("branch %q is already checked out in active worktree %q; remove it first with: git worktree remove %s", branch, wtPath, wtPath)
	}

	// Worktree directory is gone — prune stale entries and retry.
	if err := WorktreePrune(ctx, repoDir); err != nil {
		return fmt.Errorf("pruning stale worktrees: %w (original error: %v)", err, origErr)
	}
	return retry()
}

// BranchDelete deletes a local branch.
func BranchDelete(ctx context.Context, repoDir, branch string) error {
	_, err := runGit(ctx, repoDir, "branch", "-D", branch)
	return err
}

// EnsureDataRef ensures the custom data ref exists. Creates it with an empty
// initial commit if it doesn't. Uses git plumbing so nothing in the working
// tree is touched.
func EnsureDataRef(ctx context.Context, repoDir, dataRef string) error {
	_, err := runGit(ctx, repoDir, "rev-parse", "--verify", dataRef)
	if err == nil {
		return nil // already exists
	}

	// Create empty tree
	emptyTree, err := runGit(ctx, repoDir, "hash-object", "-t", "tree", "/dev/null")
	if err != nil {
		return fmt.Errorf("creating empty tree: %w", err)
	}

	// Create initial commit
	initCommit, err := runGitStdin(ctx, repoDir, "Initialize klaus run data", "commit-tree", emptyTree)
	if err != nil {
		return fmt.Errorf("creating initial commit: %w", err)
	}

	// Update ref
	_, err = runGit(ctx, repoDir, "update-ref", dataRef, initCommit)
	return err
}

// SyncToDataRef commits files to the data ref using a temporary index.
// files is a map of path-in-tree -> local-file-path.
func SyncToDataRef(ctx context.Context, repoDir, dataRef, commitMsg string, files map[string]string) error {
	if err := EnsureDataRef(ctx, repoDir, dataRef); err != nil {
		return err
	}

	parentCommit, err := runGit(ctx, repoDir, "rev-parse", dataRef)
	if err != nil {
		return fmt.Errorf("resolving data ref: %w", err)
	}

	tmpIndex, err := os.CreateTemp("", "klaus-index-*")
	if err != nil {
		return fmt.Errorf("creating temp index: %w", err)
	}
	tmpIndexPath := tmpIndex.Name()
	tmpIndex.Close()
	defer os.Remove(tmpIndexPath)

	// Read current tree into temp index
	if err := runGitEnv(ctx, repoDir, map[string]string{"GIT_INDEX_FILE": tmpIndexPath}, "read-tree", dataRef); err != nil {
		return fmt.Errorf("reading tree: %w", err)
	}

	// Add each file
	for treePath, localPath := range files {
		blob, err := runGit(ctx, repoDir, "hash-object", "-w", localPath)
		if err != nil {
			return fmt.Errorf("hashing %s: %w", localPath, err)
		}
		cacheInfo := fmt.Sprintf("100644,%s,%s", blob, treePath)
		if err := runGitEnv(ctx, repoDir, map[string]string{"GIT_INDEX_FILE": tmpIndexPath}, "update-index", "--add", "--cacheinfo", cacheInfo); err != nil {
			return fmt.Errorf("updating index for %s: %w", treePath, err)
		}
	}

	// Write tree
	newTree, err := runGitOutput(ctx, repoDir, map[string]string{"GIT_INDEX_FILE": tmpIndexPath}, "write-tree")
	if err != nil {
		return fmt.Errorf("writing tree: %w", err)
	}

	// Create commit
	newCommit, err := runGitStdin(ctx, repoDir, commitMsg, "commit-tree", newTree, "-p", parentCommit)
	if err != nil {
		return fmt.Errorf("creating commit: %w", err)
	}

	// Update ref
	_, err = runGit(ctx, repoDir, "update-ref", dataRef, newCommit)
	return err
}

// ghProtocol caches the result of detecting the user's preferred git protocol.
var (
	ghProtocolOnce sync.Once
	ghProtocolSSH  bool
)

// DetectGHProtocol runs "gh config get git_protocol" and returns true if SSH.
var detectGHProtocol = defaultDetectGHProtocol

// defaultDetectGHProtocol is the real implementation.
func defaultDetectGHProtocol() bool {
	out, err := exec.Command("gh", "config", "get", "git_protocol").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "ssh"
}

// SetDetectGHProtocol overrides the protocol detection function (for testing).
func SetDetectGHProtocol(fn func() bool) {
	detectGHProtocol = fn
}

// ResetDetectGHProtocol restores the default protocol detection function.
func ResetDetectGHProtocol() {
	detectGHProtocol = defaultDetectGHProtocol
}

// useSSHProtocol returns true if the user has configured gh to use SSH.
func useSSHProtocol() bool {
	ghProtocolOnce.Do(func() {
		ghProtocolSSH = detectGHProtocol()
	})
	return ghProtocolSSH
}

// CloneURL returns the git clone URL for a GitHub repo, respecting the user's
// configured git protocol (via gh config).
func CloneURL(owner, repo string) string {
	if useSSHProtocol() {
		return fmt.Sprintf("git@github.com:%s/%s.git", owner, repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
}

// CleanGitHubRef strips GitHub URL prefixes, .git suffix, and trailing slashes
// from a repo reference, returning the bare "owner/repo" or short name form.
func CleanGitHubRef(ref string) string {
	ref = strings.TrimSuffix(ref, ".git")

	switch {
	case strings.HasPrefix(ref, "https://github.com/"):
		ref = strings.TrimPrefix(ref, "https://github.com/")
	case strings.HasPrefix(ref, "http://github.com/"):
		ref = strings.TrimPrefix(ref, "http://github.com/")
	case strings.HasPrefix(ref, "git@github.com:"):
		ref = strings.TrimPrefix(ref, "git@github.com:")
	}

	return strings.TrimRight(ref, "/")
}

// ParseRepoRef parses a GitHub repo reference into owner, repo name, and clone URL.
// Accepts: "owner/repo", "https://github.com/owner/repo[.git]", "git@github.com:owner/repo[.git]"
func ParseRepoRef(ref string) (owner, repo, cloneURL string, err error) {
	ref = CleanGitHubRef(ref)
	parts := strings.SplitN(ref, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("invalid repo reference %q: expected owner/repo", ref)
	}

	owner = parts[0]
	repo = parts[1]
	if strings.Contains(owner, "..") || strings.Contains(repo, "..") {
		return "", "", "", fmt.Errorf("invalid repo reference %q: path traversal not allowed", ref)
	}
	cloneURL = CloneURL(owner, repo)
	return owner, repo, cloneURL, nil
}

// EnsureClone clones a repo to destDir if it doesn't already exist.
// If it already exists, fetches the latest from origin.
func EnsureClone(ctx context.Context, cloneURL, destDir string) error {
	if _, err := os.Stat(filepath.Join(destDir, ".git")); err == nil {
		_, fetchErr := runGitNetwork(ctx, destDir, "fetch", "origin", "--prune", "--tags", "--quiet")
		return fetchErr
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return fmt.Errorf("creating parent dir: %w", err)
	}
	_, err := runGitNetwork(ctx, "", "clone", cloneURL, destDir, "--quiet")
	return err
}

// PushDataRef pushes the data ref to origin.
func PushDataRef(ctx context.Context, repoDir, dataRef string) error {
	refspec := fmt.Sprintf("%s:%s", dataRef, dataRef)
	_, err := runGitNetwork(ctx, repoDir, "push", "origin", refspec, "--quiet")
	return err
}

// commitMsgHookScript is a git commit-msg hook that strips Co-Authored-By
// trailers referencing Claude or Anthropic, and removes AI attribution lines
// from commit messages. Legitimate human co-author lines are preserved.
const commitMsgHookScript = `#!/bin/sh
# Installed by klaus — strips Claude/Anthropic attribution from commit messages.
sed -i.bak \
  -e '/^Co-[Aa]uthored-[Bb]y:.*[Cc]laude/d' \
  -e '/^Co-[Aa]uthored-[Bb]y:.*[Aa]nthropic/d' \
  -e '/^[Gg]enerated.*[Cc]laude/d' \
  -e '/^[Gg]enerated.*[Aa]nthropic/d' \
  -e '/🤖.*[Cc]laude/d' \
  "$1"
rm -f "$1.bak"
`

// InstallCommitMsgHook installs a commit-msg hook in the given worktree that
// strips Claude/Anthropic attribution from commit messages.
func InstallCommitMsgHook(ctx context.Context, worktreeDir string) error {
	// Find the git dir for this worktree
	gitDir, err := runGit(ctx, worktreeDir, "rev-parse", "--git-dir")
	if err != nil {
		return fmt.Errorf("finding git dir: %w", err)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreeDir, gitDir)
	}

	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("creating hooks dir: %w", err)
	}

	hookPath := filepath.Join(hooksDir, "commit-msg")
	if err := os.WriteFile(hookPath, []byte(commitMsgHookScript), 0o755); err != nil {
		return fmt.Errorf("writing commit-msg hook: %w", err)
	}

	// Worktrees use the main repo's hooks by default. Point this worktree
	// at its own hooks directory so our commit-msg hook actually runs.
	if _, err := runGit(ctx, worktreeDir, "config", "core.hooksPath", hooksDir); err != nil {
		return fmt.Errorf("setting core.hooksPath: %w", err)
	}

	return nil
}

// ensureTimeout returns a context with the given default timeout applied if
// the parent context has no deadline set.
func ensureTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// runGit executes a local git command and returns trimmed stdout.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := ensureTimeout(ctx, localTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("git %s timed out after %s", args[0], localTimeout)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runGitNetwork executes a git command that talks to a remote and returns trimmed stdout.
func runGitNetwork(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := ensureTimeout(ctx, networkTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("git %s timed out after %s", args[0], networkTimeout)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runGitStdin executes a git command with stdin and returns trimmed stdout.
func runGitStdin(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	ctx, cancel := ensureTimeout(ctx, localTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("git %s timed out after %s", args[0], localTimeout)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runGitEnv executes a git command with extra env vars (no stdout capture needed for some).
func runGitEnv(ctx context.Context, dir string, env map[string]string, args ...string) error {
	ctx, cancel := ensureTimeout(ctx, localTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("git %s timed out after %s", args[0], localTimeout)
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

// runGitOutput executes a git command with extra env vars and returns stdout.
func runGitOutput(ctx context.Context, dir string, env map[string]string, args ...string) (string, error) {
	ctx, cancel := ensureTimeout(ctx, localTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("git %s timed out after %s", args[0], localTimeout)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

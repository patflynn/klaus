package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
	// verifyTimeout bounds best-effort remote checks at finalize.
	verifyTimeout = 20 * time.Second
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

// FetchDataRef updates the local data ref from origin. It is best-effort:
// callers should ignore the error (there may be no remote, the ref may not
// exist remotely, or the local ref may already be current). When the local
// ref is missing and the remote has it, the ref is created locally.
func FetchDataRef(ctx context.Context, repoDir, dataRef string) error {
	_, err := runGitNetwork(ctx, repoDir, "fetch", "origin", dataRef+":"+dataRef, "--quiet")
	return err
}

// ReadDataRefFile returns the raw bytes of a file stored in the data ref tree.
// treePath is the path within the ref's tree (e.g. "sessions/<run-id>.jsonl").
// It returns an error if the ref or path does not exist. Output is not trimmed,
// so JSONL content (including trailing newlines) is preserved verbatim.
func ReadDataRefFile(ctx context.Context, repoDir, dataRef, treePath string) ([]byte, error) {
	ctx, cancel := ensureTimeout(ctx, localTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", dataRef+":"+treePath)
	if repoDir != "" {
		cmd.Dir = repoDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("git cat-file timed out after %s", localTimeout)
		}
		return nil, fmt.Errorf("reading %s from %s: %w: %s", treePath, dataRef, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// WorktreeAdd creates a new worktree at path on a new branch based on startPoint.
// If the branch is already checked out in a stale worktree, it prunes and retries once.
func WorktreeAdd(ctx context.Context, repoDir, path, branch, startPoint string) error {
	_, err := runGit(ctx, repoDir, "worktree", "add", path, "-b", branch, startPoint, "--quiet")
	if err == nil {
		return nil
	}
	return retryAfterPrune(ctx, repoDir, branch, err, false, func() error {
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
	return retryAfterPrune(ctx, repoDir, branch, err, true, func() error {
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
// If forceRemoveLive is true and the worktree directory still exists, it
// force-removes the old worktree before retrying. Otherwise it returns a clear error.
func retryAfterPrune(ctx context.Context, repoDir, branch string, origErr error, forceRemoveLive bool, retry func() error) error {
	// Check whether this branch is recorded in any worktree.
	wtPath := worktreePathForBranch(ctx, repoDir, branch)
	if wtPath == "" {
		// Branch is not in any worktree — nothing to prune; return the original error.
		return origErr
	}

	if _, err := os.Stat(wtPath); err == nil {
		// Worktree directory still exists on disk.
		if !forceRemoveLive {
			return fmt.Errorf("branch %q is already checked out in active worktree %q; remove it first with: git worktree remove %s", branch, wtPath, wtPath)
		}
		// Force-remove the old worktree so we can reuse the branch.
		if err := WorktreeRemove(ctx, repoDir, wtPath); err != nil {
			return fmt.Errorf("removing existing worktree %q: %w (original error: %v)", wtPath, err, origErr)
		}
		return retry()
	}

	// Worktree directory is gone — prune stale entries and retry.
	if err := WorktreePrune(ctx, repoDir); err != nil {
		return fmt.Errorf("pruning stale worktrees: %w (original error: %v)", err, origErr)
	}
	return retry()
}

// CheckBranchName reports whether name can be used verbatim as a new branch
// name, by git check-ref-format --branch. A name git would expand, such as
// @{-1}, is rejected too.
func CheckBranchName(ctx context.Context, name string) error {
	out, err := runGit(ctx, "", "check-ref-format", "--branch", name)
	if err != nil {
		return fmt.Errorf("%q is not a valid branch name", name)
	}
	if out != name {
		return fmt.Errorf("%q is not a valid branch name (git reads it as %q)", name, out)
	}
	return nil
}

// RefExists reports whether ref resolves to an object in repoDir.
func RefExists(ctx context.Context, repoDir, ref string) bool {
	_, err := runGit(ctx, repoDir, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// BranchDelete deletes a local branch.
func BranchDelete(ctx context.Context, repoDir, branch string) error {
	_, err := runGit(ctx, repoDir, "branch", "-D", branch)
	return err
}

// IsClean reports whether the working tree at repoDir has no uncommitted changes.
// Returns true only when git status --porcelain produces empty output.
func IsClean(ctx context.Context, repoDir string) (bool, error) {
	out, err := runGit(ctx, repoDir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// CurrentBranch returns the short name of the currently checked-out branch,
// or "" if the repo is in a detached HEAD state. Any other failure (not a git
// repo, missing HEAD, etc.) is returned as an error rather than silently
// treated as "no branch".
func CurrentBranch(ctx context.Context, repoDir string) (string, error) {
	ctx, cancel := ensureTimeout(ctx, localTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return strings.TrimSpace(stdout.String()), nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git symbolic-ref timed out after %s", localTimeout)
	}
	// With --quiet, symbolic-ref exits with code 1 ONLY when HEAD is not a
	// symbolic ref (i.e., detached HEAD). Other failures (e.g., not a git
	// repo → "fatal:" exit 128) still need to bubble up.
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && stderr.Len() == 0 {
		return "", nil
	}
	return "", fmt.Errorf("git symbolic-ref: %w: %s", err, stderr.String())
}

// HasUpstream reports whether the current branch at repoDir has a tracking
// upstream. Missing upstream (a normal state) is reported as (false, nil);
// other failures — not a git repo, bad path, I/O errors — return an error.
func HasUpstream(ctx context.Context, repoDir string) (bool, error) {
	ctx, cancel := ensureTimeout(ctx, localTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return strings.TrimSpace(stdout.String()) != "", nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return false, fmt.Errorf("git rev-parse timed out after %s", localTimeout)
	}
	// Git reports missing upstream with specific stderr text and exit 128.
	// Distinguish that from fatal errors like "not a git repository".
	se := stderr.String()
	if isNoUpstreamErr(se) {
		return false, nil
	}
	return false, fmt.Errorf("git rev-parse: %w: %s", err, se)
}

// isNoUpstreamErr returns true if stderr matches the messages git emits when
// a branch has no configured upstream. Covers both `@{u}` phrasings seen in
// modern git.
func isNoUpstreamErr(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no upstream configured") ||
		strings.Contains(s, "does not point to a branch") ||
		strings.Contains(s, "head does not point")
}

// MergeFastForward runs `git merge --ff-only @{u}` in repoDir. Fails if the
// current branch has diverged from upstream, has no upstream, or the working
// tree isn't clean. Does NOT touch the tree on failure.
func MergeFastForward(ctx context.Context, repoDir string) error {
	_, err := runGit(ctx, repoDir, "merge", "--ff-only", "--quiet", "@{u}")
	return err
}

// CommitsBehindUpstream returns how many commits the current branch is behind
// its upstream. Returns 0 if already up-to-date. Assumes an upstream exists;
// callers should check HasUpstream first.
func CommitsBehindUpstream(ctx context.Context, repoDir string) (int, error) {
	out, err := runGit(ctx, repoDir, "rev-list", "--count", "HEAD..@{u}")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parsing rev-list --count output %q: %w", out, err)
	}
	return n, nil
}

// CommitsAhead returns the number of commits on ref not reachable from base.
func CommitsAhead(ctx context.Context, repoDir, base, ref string) (int, error) {
	return revListCount(ctx, repoDir, base+".."+ref)
}

// BranchPushed reports whether origin's branch tip equals the local tip. It
// asks the remote (ls-remote), not cached origin/* refs; an error means
// unverified.
func BranchPushed(ctx context.Context, repoDir, branch string) (bool, error) {
	local, err := runGit(ctx, repoDir, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	remote, err := RemoteBranchSHA(ctx, repoDir, branch)
	if err != nil {
		return false, err
	}
	return remote == local, nil
}

// RemoteBranchSHA returns origin's tip of branch, or "" when origin has no
// such branch. It asks the remote (ls-remote), not cached origin/* refs.
func RemoteBranchSHA(ctx context.Context, repoDir, branch string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	out, err := runGitNetwork(ctx, repoDir, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	sha, _, _ := strings.Cut(out, "\t")
	return sha, nil
}

func revListCount(ctx context.Context, repoDir string, args ...string) (int, error) {
	out, err := runGit(ctx, repoDir, append([]string{"rev-list", "--count"}, args...)...)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parsing rev-list --count output %q: %w", out, err)
	}
	return n, nil
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

// githubURLPrefixes are the URL forms that name a repository on github.com.
var githubURLPrefixes = []string{
	"https://github.com/",
	"http://github.com/",
	"git@github.com:",
	"ssh://git@github.com/",
}

// cutGitHubPrefix strips a github.com URL prefix from ref, reporting whether
// it had one.
func cutGitHubPrefix(ref string) (string, bool) {
	for _, p := range githubURLPrefixes {
		if rest, ok := strings.CutPrefix(ref, p); ok {
			return rest, true
		}
	}
	return ref, false
}

// CleanGitHubRef strips GitHub URL prefixes, .git suffix, and trailing slashes
// from a repo reference, returning the bare "owner/repo" or short name form.
func CleanGitHubRef(ref string) string {
	ref = strings.TrimSuffix(ref, ".git")
	ref, _ = cutGitHubPrefix(ref)
	return strings.TrimRight(ref, "/")
}

// IsGitHubRemote reports whether remote, a git remote URL, names an owner/repo
// on github.com (https, scp-style ssh, or ssh:// form). A bare "owner/repo"
// reference is not a remote URL and reports false.
func IsGitHubRemote(remote string) bool {
	if _, ok := cutGitHubPrefix(remote); !ok {
		return false
	}
	_, _, err := SplitRepoRef(remote)
	return err == nil
}

// ParseRepoRef parses a GitHub repo reference into owner, repo name, and clone URL.
// Accepts: "owner/repo", "https://github.com/owner/repo[.git]", "git@github.com:owner/repo[.git]",
// "ssh://git@github.com/owner/repo[.git]"
func ParseRepoRef(ref string) (owner, repo, cloneURL string, err error) {
	owner, repo, err = SplitRepoRef(ref)
	if err != nil {
		return "", "", "", err
	}
	return owner, repo, CloneURL(owner, repo), nil
}

// SplitRepoRef is ParseRepoRef without the clone URL, which may shell out to gh.
func SplitRepoRef(ref string) (owner, repo string, err error) {
	ref = CleanGitHubRef(ref)
	parts := strings.SplitN(ref, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo reference %q: expected owner/repo", ref)
	}

	owner = parts[0]
	repo = parts[1]
	if strings.Contains(owner, "..") || strings.Contains(repo, "..") {
		return "", "", fmt.Errorf("invalid repo reference %q: path traversal not allowed", ref)
	}
	return owner, repo, nil
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

// Provenance names the run that makes a worktree's commits and the change
// they belong to. Its trailers label commits for finding and auditing; they
// grant nothing.
type Provenance struct {
	RunID    string // Klaus-Run trailer; empty for none
	ChangeID string // Change-Id trailer; empty for none
}

// Trailers returns the provenance trailer lines for the fields that are set,
// Change-Id first, or "" when neither is.
func (p Provenance) Trailers() string {
	var lines []string
	if p.ChangeID != "" {
		lines = append(lines, "Change-Id: "+p.ChangeID)
	}
	if p.RunID != "" {
		lines = append(lines, "Klaus-Run: "+p.RunID)
	}
	return strings.Join(lines, "\n")
}

// hookConfigName is the file beside klaus's commit-msg hook that holds the
// hook's data, in git config format: the trailer values and the path of the
// hook it chains to. The script reads it with git config and never has a
// value spliced into its code.
const hookConfigName = "klaus.config"

// commitMsgHookScript is klaus's commit-msg hook. It strips Co-Authored-By
// trailers referencing Claude or Anthropic and removes AI attribution lines
// (human co-author lines are kept), adds the provenance trailers named in
// klaus.config, then runs the commit-msg hook git would otherwise have run.
// A Change-Id already on the message is kept; a Klaus-Run is added unless
// that same run is already listed, so an amend by a later run lists both.
//
// A message that is empty, or made only of lines starting with #, gets no
// trailers. Git keeps # lines for git commit -m but strips them from a message
// written in the editor, and a hook cannot tell which cleanup will apply.
// Treating such a message as empty keeps an empty editor message aborting the
// commit; stamping it would turn that abort into a commit of trailers alone.
const commitMsgHookScript = `#!/bin/sh
# Installed by klaus: strips Claude/Anthropic attribution, adds provenance
# trailers, then runs the repository's own commit-msg hook.
sed -i.bak \
  -e '/^Co-[Aa]uthored-[Bb]y:.*[Cc]laude/d' \
  -e '/^Co-[Aa]uthored-[Bb]y:.*[Aa]nthropic/d' \
  -e '/^[Gg]enerated.*[Cc]laude/d' \
  -e '/^[Gg]enerated.*[Aa]nthropic/d' \
  -e '/🤖.*[Cc]laude/d' \
  "$1"
rm -f "$1.bak"

conf="$(dirname "$0")/` + hookConfigName + `"
setting() { git config --file "$conf" --get "klaus.$1"; }

# A message with no text aborts the commit; trailers alone must not save it.
# A message made only of # lines counts as empty too, since an editor commit
# strips them. Lines below a verbose commit's scissors line are the diff.
if sed '/^. -\{24\} >8 -\{24\}$/,$d' "$1" | git stripspace --strip-comments | grep -q .; then
  change=$(setting changeid)
  if [ -n "$change" ]; then
    git interpret-trailers --in-place --no-divider \
      --if-exists doNothing --trailer "Change-Id: $change" "$1" || exit 1
  fi
  run=$(setting run)
  if [ -n "$run" ]; then
    git interpret-trailers --in-place --no-divider \
      --if-exists addIfDifferent --trailer "Klaus-Run: $run" "$1" || exit 1
  fi
fi

next=$(setting chainhook)
if [ -n "$next" ] && [ -x "$next" ]; then
  exec "$next" "$@"
fi
`

// InstallCommitMsgHook installs klaus's commit-msg hook in the linked worktree
// at worktreeDir. The hook strips Claude/Anthropic attribution, adds prov's
// trailers (none when prov is zero), and then runs the commit-msg hook the
// repository would otherwise run. It is idempotent.
//
// The hook lives in the worktree's own git dir and is enabled with a
// worktree-scoped core.hooksPath, so other worktrees and the main clone keep
// their own hooks. That needs extensions.worktreeConfig, which this enables on
// the repository once. A shared core.hooksPath that points into a worktree's
// git dir, as earlier klaus versions left behind, is removed.
func InstallCommitMsgHook(ctx context.Context, worktreeDir string, prov Provenance) error {
	for _, v := range []string{prov.RunID, prov.ChangeID} {
		if strings.ContainsAny(v, " \t\r\n") {
			return fmt.Errorf("provenance value %q must be a single token", v)
		}
	}
	gitDir, err := absGitPath(ctx, worktreeDir, "--git-dir")
	if err != nil {
		return fmt.Errorf("finding git dir: %w", err)
	}
	commonDir, err := absGitPath(ctx, worktreeDir, "--git-common-dir")
	if err != nil {
		return fmt.Errorf("finding common git dir: %w", err)
	}
	// In the main worktree the git dir's hooks/ is the repository's own.
	if samePath(gitDir, commonDir) {
		return fmt.Errorf("%s is not a linked worktree", worktreeDir)
	}

	if err := removeLeakedHooksPath(ctx, worktreeDir, commonDir); err != nil {
		return err
	}
	chain := chainedCommitMsgHook(ctx, worktreeDir, commonDir)

	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("creating hooks dir: %w", err)
	}
	conf := filepath.Join(hooksDir, hookConfigName)
	if err := os.Remove(conf); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replacing %s: %w", hookConfigName, err)
	}
	for _, kv := range [][2]string{{"run", prov.RunID}, {"changeid", prov.ChangeID}, {"chainhook", chain}} {
		if kv[1] == "" {
			continue
		}
		if _, err := runGit(ctx, worktreeDir, "config", "--file", conf, "klaus."+kv[0], kv[1]); err != nil {
			return fmt.Errorf("writing %s: %w", hookConfigName, err)
		}
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "commit-msg"), []byte(commitMsgHookScript), 0o755); err != nil {
		return fmt.Errorf("writing commit-msg hook: %w", err)
	}

	if err := enableWorktreeConfig(ctx, worktreeDir); err != nil {
		return err
	}
	if _, err := runGit(ctx, worktreeDir, "config", "--worktree", "core.hooksPath", hooksDir); err != nil {
		return fmt.Errorf("setting core.hooksPath: %w", err)
	}
	return nil
}

// absGitPath runs git rev-parse with flag (--git-dir, --git-common-dir) in dir
// and returns the path it names, made absolute.
func absGitPath(ctx context.Context, dir, flag string) (string, error) {
	p, err := runGit(ctx, dir, "rev-parse", flag)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p), nil
}

// enableWorktreeConfig turns on extensions.worktreeConfig in the shared
// repository config, so git config --worktree has a file of its own. Git
// honours the extension in format-0 repositories too.
func enableWorktreeConfig(ctx context.Context, dir string) error {
	enabled := func() bool {
		v, err := runGit(ctx, dir, "config", "--local", "--type=bool", "--get", "extensions.worktreeConfig")
		return err == nil && v == "true"
	}
	if err := updateSharedConfig(ctx, dir, enabled, "config", "--local", "extensions.worktreeConfig", "true"); err != nil {
		return fmt.Errorf("enabling extensions.worktreeConfig: %w", err)
	}
	return nil
}

// removeLeakedHooksPath removes a shared core.hooksPath that points inside the
// common git dir's worktrees/. Earlier klaus versions wrote one there, which
// made every worktree of the clone run the last-set-up worktree's hooks, and
// none at all once that worktree was cleaned up. Any other value is the
// repository's own choice and is kept.
func removeLeakedHooksPath(ctx context.Context, dir, commonDir string) error {
	leaked := func() []string {
		out, err := runGit(ctx, dir, "config", "--local", "--get-all", "core.hooksPath")
		if err != nil {
			return nil // unset
		}
		var vs []string
		for _, v := range strings.Split(out, "\n") {
			if inWorktreesDir(v, commonDir) {
				vs = append(vs, v)
			}
		}
		return vs
	}
	for _, v := range leaked() {
		gone := func() bool { return !slices.Contains(leaked(), v) }
		if err := updateSharedConfig(ctx, dir, gone, "config", "--local", "--fixed-value", "--unset-all", "core.hooksPath", v); err != nil {
			return fmt.Errorf("removing shared core.hooksPath %s: %w", v, err)
		}
	}
	return nil
}

// configLockWait bounds how long updateSharedConfig keeps retrying while
// another process holds the shared config's lock.
var configLockWait = 2 * time.Second

// updateSharedConfig runs git with args in dir to change the shared config,
// unless done reports the change is already there. git config fails at once
// when another process holds config.lock, as a concurrent launch does while
// it makes the same change, and that launch may not have renamed its new
// config into place yet. So a failed write is retried with a short backoff,
// re-checking done after each wait, and fails only once configLockWait has
// passed.
func updateSharedConfig(ctx context.Context, dir string, done func() bool, args ...string) error {
	deadline := time.Now().Add(configLockWait)
	wait := 10 * time.Millisecond
	for {
		if done() {
			return nil
		}
		_, err := runGit(ctx, dir, args...)
		if err == nil {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(min(wait, remaining)):
		}
		wait = min(2*wait, 200*time.Millisecond)
	}
}

// chainedCommitMsgHook returns the commit-msg hook git would run in the
// worktree at dir without klaus's: under the core.hooksPath set outside the
// worktree's own config, else under the common git dir's hooks/. It returns ""
// when that path is inside a worktree's git dir: another run's klaus hook.
func chainedCommitMsgHook(ctx context.Context, dir, commonDir string) string {
	hooksDir := filepath.Join(commonDir, "hooks")
	// --show-scope lists values lowest precedence first; the last one wins.
	if out, err := runGit(ctx, dir, "config", "--show-scope", "--type=path", "--get-all", "core.hooksPath"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			scope, v, ok := strings.Cut(line, "\t")
			if !ok || scope == "worktree" || scope == "command" {
				continue
			}
			hooksDir = v
		}
	}
	// A relative core.hooksPath is relative to the worktree root, where git
	// runs hooks.
	if !filepath.IsAbs(hooksDir) {
		hooksDir = filepath.Join(dir, hooksDir)
	}
	if inWorktreesDir(hooksDir, commonDir) {
		return ""
	}
	return filepath.Join(hooksDir, "commit-msg")
}

// inWorktreesDir reports whether the absolute path p is inside the common git
// dir's worktrees/, where linked worktrees keep their git dirs.
func inWorktreesDir(p, commonDir string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	roots := []string{commonDir}
	if real, err := filepath.EvalSymlinks(commonDir); err == nil {
		roots = append(roots, real)
	}
	for _, root := range roots {
		rel, err := filepath.Rel(filepath.Join(root, "worktrees"), p)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// samePath reports whether a and b name the same directory.
func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// CommitChangeID returns the Change-Id trailer of commit rev in dir, or ""
// when it has none.
func CommitChangeID(ctx context.Context, dir, rev string) (string, error) {
	out, err := runGit(ctx, dir, "log", "-1", "--format=%(trailers:key=Change-Id,valueonly,separator=%x0A)", rev)
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(first), nil
}

// ensureTimeout returns a context with the given default timeout applied if
// the parent context has no deadline set.
func ensureTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
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

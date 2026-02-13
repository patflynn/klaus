package git

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoRoot returns the top-level directory of the git repository.
func RepoRoot() (string, error) {
	return runGit("", "rev-parse", "--show-toplevel")
}

// CommonDir returns the absolute path to the git common directory.
// This works from worktrees too (returns the main repo's .git dir).
func CommonDir() (string, error) {
	d, err := runGit("", "rev-parse", "--git-common-dir")
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

// FetchBranch fetches a branch from origin.
func FetchBranch(repoDir, branch string) error {
	_, err := runGit(repoDir, "fetch", "origin", branch, "--quiet")
	return err
}

// WorktreeAdd creates a new worktree at path on a new branch based on startPoint.
func WorktreeAdd(repoDir, path, branch, startPoint string) error {
	_, err := runGit(repoDir, "worktree", "add", path, "-b", branch, startPoint, "--quiet")
	return err
}

// WorktreeRemove removes a worktree. repoDir is the main repo directory.
func WorktreeRemove(repoDir, path string) error {
	_, err := runGit(repoDir, "worktree", "remove", "--force", path)
	return err
}

// BranchDelete deletes a local branch.
func BranchDelete(repoDir, branch string) error {
	_, err := runGit(repoDir, "branch", "-D", branch)
	return err
}

// EnsureDataRef ensures the custom data ref exists. Creates it with an empty
// initial commit if it doesn't. Uses git plumbing so nothing in the working
// tree is touched.
func EnsureDataRef(repoDir, dataRef string) error {
	_, err := runGit(repoDir, "rev-parse", "--verify", dataRef)
	if err == nil {
		return nil // already exists
	}

	// Create empty tree
	emptyTree, err := runGit(repoDir, "hash-object", "-t", "tree", "/dev/null")
	if err != nil {
		return fmt.Errorf("creating empty tree: %w", err)
	}

	// Create initial commit
	initCommit, err := runGitStdin(repoDir, "Initialize klaus run data", "commit-tree", emptyTree)
	if err != nil {
		return fmt.Errorf("creating initial commit: %w", err)
	}

	// Update ref
	_, err = runGit(repoDir, "update-ref", dataRef, initCommit)
	return err
}

// SyncToDataRef commits files to the data ref using a temporary index.
// files is a map of path-in-tree -> local-file-path.
func SyncToDataRef(repoDir, dataRef, commitMsg string, files map[string]string) error {
	if err := EnsureDataRef(repoDir, dataRef); err != nil {
		return err
	}

	parentCommit, err := runGit(repoDir, "rev-parse", dataRef)
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
	if err := runGitEnv(repoDir, map[string]string{"GIT_INDEX_FILE": tmpIndexPath}, "read-tree", dataRef); err != nil {
		return fmt.Errorf("reading tree: %w", err)
	}

	// Add each file
	for treePath, localPath := range files {
		blob, err := runGit(repoDir, "hash-object", "-w", localPath)
		if err != nil {
			return fmt.Errorf("hashing %s: %w", localPath, err)
		}
		cacheInfo := fmt.Sprintf("100644,%s,%s", blob, treePath)
		if err := runGitEnv(repoDir, map[string]string{"GIT_INDEX_FILE": tmpIndexPath}, "update-index", "--add", "--cacheinfo", cacheInfo); err != nil {
			return fmt.Errorf("updating index for %s: %w", treePath, err)
		}
	}

	// Write tree
	newTree, err := runGitOutput(repoDir, map[string]string{"GIT_INDEX_FILE": tmpIndexPath}, "write-tree")
	if err != nil {
		return fmt.Errorf("writing tree: %w", err)
	}

	// Create commit
	newCommit, err := runGitStdin(repoDir, commitMsg, "commit-tree", newTree, "-p", parentCommit)
	if err != nil {
		return fmt.Errorf("creating commit: %w", err)
	}

	// Update ref
	_, err = runGit(repoDir, "update-ref", dataRef, newCommit)
	return err
}

// PushDataRef pushes the data ref to origin.
func PushDataRef(repoDir, dataRef string) error {
	refspec := fmt.Sprintf("%s:%s", dataRef, dataRef)
	_, err := runGit(repoDir, "push", "origin", refspec, "--quiet")
	return err
}

// runGit executes a git command and returns trimmed stdout.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runGitStdin executes a git command with stdin and returns trimmed stdout.
func runGitStdin(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runGitEnv executes a git command with extra env vars (no stdout capture needed for some).
func runGitEnv(dir string, env map[string]string, args ...string) error {
	cmd := exec.Command("git", args...)
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
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

// runGitOutput executes a git command with extra env vars and returns stdout.
func runGitOutput(dir string, env map[string]string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
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
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

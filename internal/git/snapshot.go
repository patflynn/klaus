package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// SyncSnapshot makes path a detached worktree of repoDir checked out at
// commit. When path is already one of repoDir's worktrees it moves it to
// commit, discarding anything written there; otherwise it replaces path with
// a new worktree. A snapshot holds no branch, so it never keeps a launch from
// checking one out.
func SyncSnapshot(ctx context.Context, repoDir, path, commit string) error {
	if snapshotOf(ctx, repoDir, path) {
		if _, err := runGit(ctx, path, "checkout", "--quiet", "--detach", "--force", commit); err != nil {
			return err
		}
		_, err := runGit(ctx, path, "clean", "-ffdxq")
		return err
	}
	if err := RemoveSnapshot(ctx, path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Prune first: a snapshot whose directory was deleted is still
	// registered at path, and git refuses to add a worktree there.
	if _, err := runGit(ctx, repoDir, "worktree", "prune"); err != nil {
		return err
	}
	_, err := runGit(ctx, repoDir, "worktree", "add", "--quiet", "--detach", path, commit)
	return err
}

// SnapshotCommit returns the commit the snapshot at path has checked out,
// or "" when there is no snapshot there.
func SnapshotCommit(ctx context.Context, path string) string {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return ""
	}
	return RefCommit(ctx, path, "HEAD")
}

// RemoveSnapshot removes the snapshot at path and its registration in the
// clone it came from. A path that isn't a worktree is removed outright.
func RemoveSnapshot(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		if common, err := runGit(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
			_, _ = runGit(ctx, path, "worktree", "remove", "--force", path)
			defer func() { _, _ = runGit(ctx, "", "--git-dir="+common, "worktree", "prune") }()
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("removing snapshot %s: %w", path, err)
	}
	return nil
}

// snapshotOf reports whether path is the top of a worktree of repoDir.
func snapshotOf(ctx context.Context, repoDir, path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return false
	}
	top, err := runGit(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil || !sameFile(top, path) {
		return false
	}
	theirs, err := runGit(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false
	}
	ours, err := runGit(ctx, repoDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	return err == nil && sameFile(theirs, ours)
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

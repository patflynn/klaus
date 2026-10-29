package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/consult"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/project"
)

// repoSnapshot is the workspace a --repo consult reads: a detached worktree
// of the project's registered clone at origin's default branch, never the
// clone's own working tree.
type repoSnapshot struct {
	project, branch, dir, commit string
}

// workspaceLine heads the prompt, so the partner can cite what it read.
func (s *repoSnapshot) workspaceLine() string {
	return fmt.Sprintf("Workspace: %s at origin/%s %s", s.project, s.branch, s.commit)
}

// prepareRepoSnapshot fetches origin's default branch into the clone of the
// registered project name and moves the snapshot at dir to it, as launch
// starts a worktree: the target config's default_branch, else main. When the
// fetch fails, it warns on errOut and reads the snapshot dir already holds;
// with none there, it fails.
func prepareRepoSnapshot(ctx context.Context, name, dir string, errOut io.Writer) (*repoSnapshot, error) {
	reg, err := project.Load()
	if err != nil {
		return nil, err
	}
	_, clone := resolveRepoTarget(name, "", reg)
	if clone == "" {
		return nil, fmt.Errorf("project %q is not registered; use klaus project add or --dir", name)
	}
	s := &repoSnapshot{project: name, branch: "main", dir: dir}
	if cfg, err := config.Load(clone); err == nil && cfg.DefaultBranch != "" {
		s.branch = cfg.DefaultBranch
	}
	if fetchErr := git.FetchRemoteBranch(ctx, clone, s.branch); fetchErr != nil {
		s.commit = git.SnapshotCommit(ctx, dir)
		if s.commit == "" {
			return nil, fmt.Errorf("fetching origin/%s of project %q: %w; there is no earlier snapshot to read", s.branch, name, fetchErr)
		}
		fmt.Fprintf(errOut, "warning: fetching origin/%s of project %q failed: %v\nreading the last snapshot, at %s\n",
			s.branch, name, strings.TrimSpace(fetchErr.Error()), s.commit)
		return s, nil
	}
	s.commit = git.RefCommit(ctx, clone, "refs/remotes/origin/"+s.branch)
	if s.commit == "" {
		return nil, fmt.Errorf("project %q has no origin/%s", name, s.branch)
	}
	if err := git.SyncSnapshot(ctx, clone, dir, s.commit); err != nil {
		return nil, fmt.Errorf("checking out origin/%s of project %q at %s: %w", s.branch, name, dir, err)
	}
	return s, nil
}

// oneShotSnapshotDir is a new, empty directory for the snapshot of an
// unthreaded --repo consult, under ~/.klaus/consult-snapshots. The snapshot
// is the project-named directory inside it, so remove both when done.
func oneShotSnapshotDir(name string) (parent, dir string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	root := filepath.Join(home, ".klaus", "consult-snapshots")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", err
	}
	parent, err = os.MkdirTemp(root, "consult-*")
	if err != nil {
		return "", "", err
	}
	// A name such as ".." must not lead outside parent, which goes with it.
	base := filepath.Base(name)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		base = "workspace"
	}
	return parent, filepath.Join(parent, base), nil
}

// removeConsultSnapshots removes the snapshot of every consult thread in
// store that no consult is using. Each thread makes a new one on its next
// turn, at the same path.
func removeConsultSnapshots(ctx context.Context, store consult.Store, out io.Writer) {
	threads, err := store.List()
	if err != nil {
		fmt.Fprintf(out, "  warning: listing consult threads: %v\n", err)
		return
	}
	for name := range threads {
		dir, err := store.WorkspaceDir(name)
		if err != nil {
			continue
		}
		if _, err := os.Lstat(dir); err != nil {
			continue
		}
		unlock, err := store.Lock(name)
		if err != nil {
			fmt.Fprintf(out, "skipping consult thread %s's snapshot (in use)\n", name)
			continue
		}
		if err := git.RemoveSnapshot(ctx, dir); err != nil {
			fmt.Fprintf(out, "  warning: %v\n", err)
		} else {
			fmt.Fprintf(out, "Removed consult thread %s's snapshot\n", name)
		}
		unlock()
	}
}

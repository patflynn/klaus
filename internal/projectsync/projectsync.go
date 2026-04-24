// Package projectsync keeps registered project clones current with their
// upstreams. It is deliberately conservative: it fetches every project, and
// only performs a fast-forward merge when the working tree is clean and the
// current branch has a tracking upstream. It never resets, force-updates, or
// switches branches. Any surprising state (dirty tree, detached HEAD, diverged
// branch, no upstream) is reported as a "skipped" result and left untouched.
package projectsync

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/project"
)

// PerRepoTimeout bounds the total time spent syncing a single repo.
const PerRepoTimeout = 15 * time.Second

// MaxConcurrency caps how many project syncs run at once. Each sync spawns
// several `git` subprocesses, so unbounded fan-out on a registry with many
// projects can exhaust file descriptors or thrash the disk.
const MaxConcurrency = 8

// Status is the outcome of syncing a single project.
type Status string

const (
	// StatusUpToDate means fetch ran and the branch was already current.
	StatusUpToDate Status = "up-to-date"
	// StatusPulled means a fast-forward merge advanced the working branch.
	StatusPulled Status = "pulled"
	// StatusFetched means fetch ran but no ff was attempted.
	StatusFetched Status = "fetched-only"
	// StatusSkipped means the repo was left untouched (dirty, diverged,
	// detached, or no upstream). Detail explains why.
	StatusSkipped Status = "skipped"
	// StatusError means fetch or a git query failed. Detail contains the error.
	StatusError Status = "error"
)

// SyncResult describes the outcome of syncing one project.
type SyncResult struct {
	Name   string
	Path   string
	Branch string
	Status Status
	// Detail is a short human-readable explanation. For skipped results this
	// is the reason (e.g. "dirty tree"); for errors it's the error message;
	// for pulled results it notes how many commits were applied.
	Detail string
}

// Sync fetches and fast-forwards every project in reg concurrently. Returns
// results sorted by project name. Per-project failures are reported in the
// results, never as a returned error.
//
// excludePaths lists absolute paths the caller is already operating on in the
// foreground; matching projects are skipped to avoid racing with concurrent
// git operations (e.g., FETCH_HEAD.lock contention).
func Sync(ctx context.Context, reg *project.Registry, gc git.Client, excludePaths ...string) []SyncResult {
	if reg == nil {
		return nil
	}
	projects := reg.List()
	if len(projects) == 0 {
		return nil
	}

	excluded := make(map[string]struct{}, len(excludePaths))
	for _, p := range excludePaths {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			excluded[abs] = struct{}{}
		} else {
			excluded[p] = struct{}{}
		}
	}

	resCh := make(chan SyncResult, len(projects))
	sem := make(chan struct{}, MaxConcurrency)
	var wg sync.WaitGroup
	for name, path := range projects {
		if isExcluded(path, excluded) {
			continue
		}
		wg.Add(1)
		go func(name, path string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			resCh <- syncOne(ctx, gc, name, path)
		}(name, path)
	}
	wg.Wait()
	close(resCh)

	results := make([]SyncResult, 0, len(projects))
	for r := range resCh {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results
}

// isExcluded reports whether path resolves to the same absolute path as any
// entry in excluded.
func isExcluded(path string, excluded map[string]struct{}) bool {
	if len(excluded) == 0 {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	_, ok := excluded[abs]
	return ok
}

func syncOne(ctx context.Context, gc git.Client, name, path string) SyncResult {
	r := SyncResult{Name: name, Path: path}

	ctx, cancel := context.WithTimeout(ctx, PerRepoTimeout)
	defer cancel()

	// Capture the branch for reporting even if later steps fail/skip.
	branch, _ := gc.CurrentBranch(ctx, path)
	r.Branch = branch

	if err := gc.FetchAll(ctx, path); err != nil {
		r.Status = StatusError
		r.Detail = fmt.Sprintf("fetch failed: %v", err)
		return r
	}

	if branch == "" {
		r.Status = StatusSkipped
		r.Detail = "detached HEAD"
		return r
	}

	clean, err := gc.IsClean(ctx, path)
	if err != nil {
		r.Status = StatusError
		r.Detail = fmt.Sprintf("status check failed: %v", err)
		return r
	}
	if !clean {
		r.Status = StatusSkipped
		r.Detail = "dirty tree"
		return r
	}

	hasUp, err := gc.HasUpstream(ctx, path)
	if err != nil {
		r.Status = StatusError
		r.Detail = fmt.Sprintf("upstream check failed: %v", err)
		return r
	}
	if !hasUp {
		r.Status = StatusSkipped
		r.Detail = "no upstream"
		return r
	}

	behind, err := gc.CommitsBehindUpstream(ctx, path)
	if err != nil {
		r.Status = StatusError
		r.Detail = fmt.Sprintf("rev-list failed: %v", err)
		return r
	}
	if behind == 0 {
		r.Status = StatusUpToDate
		return r
	}

	if err := gc.MergeFastForward(ctx, path); err != nil {
		// ff failed — most likely diverged. Leave the tree alone.
		r.Status = StatusSkipped
		r.Detail = fmt.Sprintf("cannot fast-forward: %v", err)
		return r
	}

	r.Status = StatusPulled
	if behind == 1 {
		r.Detail = "1 commit"
	} else {
		r.Detail = fmt.Sprintf("%d commits", behind)
	}
	return r
}

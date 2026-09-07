package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/project"
)

// errNoTargetRepo is returned when there is nothing to resolve: no repo was
// requested and the current directory is not a checkout.
var errNoTargetRepo = errors.New("not inside a git repository and no target repo given")

// repoDirCache memoizes resolveRepoDir per repo reference. Resolution can clone
// or fetch, so a command that needs the directory more than once (merging a
// queue of PRs from the same repo, say) only pays for it once per process.
var (
	repoDirMu    sync.Mutex
	repoDirCache = map[string]string{}
)

// resolveRepoDir returns a local git repository directory for repoRef
// ("owner/repo", a full GitHub URL, or a registered project name), cloning it
// under worktree_base/.repos when no local checkout exists. It mirrors how
// 'klaus launch' finds a repo, so commands that need a checkout work from
// anywhere — including a klaus session workspace that is not itself a clone.
//
// Use existingRepoDir instead when a checkout is merely nice to have: this
// function may clone, which is far too much work for, say, reading config.
func resolveRepoDir(ctx context.Context, repoRef string) (string, error) {
	repoDirMu.Lock()
	defer repoDirMu.Unlock()
	if dir, ok := repoDirCache[repoRef]; ok {
		return dir, nil
	}
	dir, err := resolveRepoDirUncached(ctx, repoRef)
	if err != nil {
		return "", err
	}
	repoDirCache[repoRef] = dir
	return dir, nil
}

func resolveRepoDirUncached(ctx context.Context, repoRef string) (string, error) {
	if dir := existingRepoDir(repoRef); dir != "" {
		return dir, nil
	}
	if repoRef == "" {
		return "", errNoTargetRepo
	}

	owner, repo, cloneURL, err := git.ParseRepoRef(repoRef)
	if err != nil {
		return "", err
	}
	cwdRoot, _ := git.RepoRoot()
	cfg, err := config.Load(cwdRoot)
	if err != nil {
		return "", fmt.Errorf("could not load configuration: %w", err)
	}
	cloneDir := filepath.Join(cfg.WorktreeBase, ".repos", owner, repo)
	if err := git.NewExecClient().EnsureClone(ctx, cloneURL, cloneDir); err != nil {
		return "", fmt.Errorf("cloning %s: %w", repoRef, err)
	}
	return cloneDir, nil
}

// existingRepoDir returns a local checkout of repoRef that already exists, or
// "" when none does. It never clones or fetches.
//
// An empty repoRef means "whatever repo the current directory is in". A
// non-empty one matches the current checkout only when their origins agree, and
// otherwise falls back to a registered project.
func existingRepoDir(repoRef string) string {
	cwdRoot, _ := git.RepoRoot() // "" outside a checkout
	if repoRef == "" {
		return cwdRoot
	}
	wantSlug := repoSlug(repoRef)
	// The current checkout counts only when it is demonstrably the requested
	// repo. A bare project name never matches it: the name says nothing about
	// which remote this checkout points at.
	if cwdRoot != "" && wantSlug != "" && strings.EqualFold(repoSlugForDir(cwdRoot), wantSlug) {
		return cwdRoot
	}
	return registeredProjectDir(repoRef, wantSlug)
}

// registeredProjectDir returns the local path of a registered project that
// holds the requested repo, or "" when none does. A project matches when its
// origin remote resolves to the same owner/repo slug. When a project's origin
// is not a GitHub URL (a local path, say), its name is matched against the repo
// portion of the reference instead — but a project whose origin points at a
// different GitHub repo is never matched by name.
func registeredProjectDir(repoRef, wantSlug string) string {
	reg, err := project.Load()
	if err != nil || reg == nil {
		return ""
	}
	projects := reg.List()

	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic pick when several projects match by name

	shortName := repoRef
	if _, after, found := strings.Cut(git.CleanGitHubRef(repoRef), "/"); found {
		shortName = after
	}

	var nameMatch string
	for _, name := range names {
		dir := projects[name]
		slug := repoSlugForDir(dir)
		if wantSlug != "" && strings.EqualFold(slug, wantSlug) {
			return dir
		}
		if slug == "" && name == shortName && nameMatch == "" {
			nameMatch = dir
		}
	}
	return nameMatch
}

// repoSlug normalizes a repo reference to "owner/repo", or "" when it has no
// owner (a bare project name) or cannot be parsed.
func repoSlug(ref string) string {
	if ref == "" || !strings.Contains(git.CleanGitHubRef(ref), "/") {
		return ""
	}
	owner, repo, _, err := git.ParseRepoRef(ref)
	if err != nil {
		return ""
	}
	return owner + "/" + repo
}

// repoSlugForDir returns the "owner/repo" slug of a checkout's origin remote,
// or "" when the directory is not a repo or its origin is not a GitHub URL.
func repoSlugForDir(dir string) string {
	if dir == "" {
		return ""
	}
	remote := gitRemoteURL(dir)
	if remote == "" {
		return ""
	}
	return repoSlug(remote)
}

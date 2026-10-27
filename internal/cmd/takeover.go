package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
)

// continuationLockWait is how long a launch that continues a branch waits
// for another such launch in the same clone to register its run.
var continuationLockWait = 5 * time.Minute

// lockContinuation takes the lock that serialises the launches continuing a
// branch (--pr, --change, or --resume-from on its run's branch) in the
// clone at repoRoot, from the check that no other run holds the branch or
// the change through the new run's registration: a second launch waits,
// then finds the first run registered and refuses. It waits up to
// continuationLockWait, saying so on stderr. The returned func releases it.
func lockContinuation(ctx context.Context, repoRoot string) (func(), error) {
	f, err := openContinuationLock(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(continuationLockWait)
	for said := false; ; said = true {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil // close drops the flock
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
		}
		if !said {
			fmt.Fprintf(os.Stderr, "waiting for another launch continuing a branch in %s to start its run...\n", repoRoot)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("another launch continuing a branch in %s still hasn't started its run after %s", repoRoot, continuationLockWait)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// tryLockContinuation takes the lock lockContinuation takes, if no launch
// holds it; ok is false when one does, or the lock can't be taken.
func tryLockContinuation(ctx context.Context, repoRoot string) (release func(), ok bool) {
	f, err := openContinuationLock(ctx, repoRoot)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return func() { f.Close() }, true
}

// openContinuationLock opens the continuation lock's file, in the clone's
// common git directory, which its worktrees share.
func openContinuationLock(ctx context.Context, repoRoot string) (*os.File, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return nil, fmt.Errorf("finding the git directory of %s: %w", repoRoot, err)
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoRoot, dir)
	}
	return os.OpenFile(filepath.Join(dir, "klaus-continue.lock"), os.O_CREATE|os.O_RDWR, 0o644)
}

// occupantError is the refusal to create a worktree on branch while the
// worktree at path has it checked out, holding work that origin lacks (""
// for none): owner is the run that registered it, or nil when none did.
// klaus never removes such a worktree, even for --force-takeover, so the
// operator frees the branch first.
func occupantError(branch, path, work string, owner *run.State) error {
	what := path
	if work != "" {
		what += " (with " + work + ")"
	}
	if owner == nil {
		return fmt.Errorf("branch %s is checked out in %s, which no klaus run registered; klaus never removes it: check another branch out there, or remove it yourself", branch, what)
	}
	free := "clean that run up first"
	if work != "" {
		free = "save that work, then clean that run up"
	}
	return fmt.Errorf("branch %s is checked out in run %s's worktree %s; klaus never removes a worktree, even with --force-takeover: %s (klaus cleanup %s, with --force while its agent runs) and launch again", branch, owner.ID, what, free, owner.ID)
}

// worktreeOwner returns the run whose recorded worktree is path, or nil.
func worktreeOwner(states []*run.State, path string) *run.State {
	clean := filepath.Clean(path)
	for _, s := range states {
		if s.Type != "session" && s.Worktree != "" && filepath.Clean(s.Worktree) == clean {
			return s
		}
	}
	return nil
}

// takeoverCandidates are the runs whose attention a run of changeID in repo
// (as integration.RepoName names it) could take on: other runs of the
// change that need attention and pushed their work, and that self doesn't
// already record.
func takeoverCandidates(states []*run.State, self *run.State, changeID string, repos ...string) []*run.State {
	var out []*run.State
	for _, s := range states {
		if s.Type == "session" || s.ID == self.ID || s.ChangeID != changeID || !run.ValidChangeID(changeID) ||
			!slices.Contains(repos, integration.RepoName(s)) || !needsAttention(s) || s.PushedSHA == nil ||
			slices.Contains(self.TookOver, s.ID) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// recordTakeovers adds to self.TookOver each candidate whose pushed head is
// head or one of its ancestors in the repo at dir: self holds that work.
// It returns the IDs it added.
func recordTakeovers(ctx context.Context, dir, head string, self *run.State, candidates []*run.State) []string {
	var added []string
	for _, s := range candidates {
		if git.IsAncestor(ctx, dir, *s.PushedSHA, head) {
			self.TookOver = append(self.TookOver, s.ID)
			added = append(added, s.ID)
		}
	}
	return added
}

// recordFinalTakeovers records, as a run finalizes, the runs of its change
// that need attention and whose pushed heads the head it left on origin
// contains: its pushed head, or for a PR its branch's tip on origin. It
// asks origin only when there is such a run to check.
func recordFinalTakeovers(ctx context.Context, gc git.Client, state *run.State) {
	if state.Worktree == "" {
		return
	}
	states, _ := run.ListAllSessions()
	candidates := takeoverCandidates(states, state, state.ChangeID, integration.RepoName(state))
	if len(candidates) == 0 {
		return
	}
	var head string
	switch {
	case state.PushedSHA != nil:
		head = *state.PushedSHA
	case state.PRURL != nil && *state.PRURL != "":
		head, _ = gc.RemoteBranchSHA(ctx, state.Worktree, state.Branch)
	}
	if head != "" {
		recordTakeovers(ctx, state.Worktree, head, state, candidates)
	}
}

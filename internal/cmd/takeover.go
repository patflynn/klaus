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
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return nil, fmt.Errorf("finding the git directory of %s: %w", repoRoot, err)
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoRoot, dir)
	}
	f, err := os.OpenFile(filepath.Join(dir, "klaus-continue.lock"), os.O_CREATE|os.O_RDWR, 0o644)
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

// approveTakeover returns what a --force-takeover onto branch, in the clone
// at repoRoot, may discard: work origin lacks on the branch, and the
// worktree that has it checked out when a run registered it, as
// git.Takeover.Occupant. It refuses while a worktree that no run
// registered, whose directory exists, has it checked out: a takeover
// replaces a run's worktree, never anything else. Creating the worktree
// removes that occupant only if it still has the branch checked out, and
// refuses if another one took it since (movedOccupantError). states are
// every recorded run.
func approveTakeover(ctx context.Context, repoRoot, branch string, states []*run.State) (*git.Takeover, error) {
	wt := git.BranchWorktree(ctx, repoRoot, branch)
	if wt == "" {
		return &git.Takeover{}, nil
	}
	if _, err := os.Stat(wt); err != nil {
		return &git.Takeover{}, nil // stale: pruned when the worktree is created
	}
	if worktreeOwner(states, wt) == nil {
		return nil, occupantError(branch, wt, nil)
	}
	return &git.Takeover{Occupant: wt}, nil
}

// occupantError is the refusal to create a worktree on branch while the
// worktree at path has it checked out: the run that registered it, owner,
// or nil when none did.
func occupantError(branch, path string, owner *run.State) error {
	if owner == nil {
		return fmt.Errorf("branch %s is checked out in %s, which no klaus run registered; klaus never removes it: check another branch out there, or remove it yourself", branch, path)
	}
	return fmt.Errorf("branch %s is checked out in run %s's worktree %s; clean that run up (klaus cleanup %s), or pass --force-takeover to replace its worktree", branch, owner.ID, path, owner.ID)
}

// movedOccupantError is a forced takeover's refusal once branch turns out
// to be checked out in path, not in the worktree the takeover approved
// removing (takeover.Occupant): another worktree took it in between.
func movedOccupantError(branch, path string, takeover *git.Takeover) error {
	was := "no worktree had it checked out"
	if takeover.Occupant != "" {
		was = "it was checked out in " + takeover.Occupant
	}
	return fmt.Errorf("branch %s is now checked out in %s, though %s when --force-takeover was checked; klaus removes only the worktree it checked, and left %s alone", branch, path, was, path)
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

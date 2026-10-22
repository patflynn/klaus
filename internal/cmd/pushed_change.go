package cmd

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
)

// pushedHead is a head on origin that a run pushed, and the change its
// Change-Id trailer names ("" when it names none klaus can use).
type pushedHead struct {
	branch, sha, changeID string
}

// recordPushedChange makes the run's change and branch the ones it actually
// pushed. Launch records a change and branch for the run, but an agent may
// leave them, as one launched to continue a change does when it makes a new
// change instead: it switches to a branch of its own, and its commits carry
// their own Change-Id.
//
// The branches finalize looks at are the one the worktree ends on and
// launch's, other than defaultBranch. A branch's head on origin counts as
// pushed by the run when the local branch contains it, as it does after a
// push from the worktree, and the head the run started from (launchHead)
// doesn't. Its change is its Change-Id trailer.
// Of the heads the run pushed, the run's change is the one its final head
// belongs to: the pushed head whose change the final head's trailer names,
// else the head of the branch the worktree ends on. Finalize records that
// change, its branch and its head, and emits agent:change-recorded when they
// aren't launch's or when the run pushed heads of more than one change.
// Moving to another change drops the runs launch recorded as taken over
// (TookOver), which are the launch change's. When the run pushed nothing,
// or nothing that names another branch or change, launch's values stay.
func recordPushedChange(ctx context.Context, baseDir string, state *run.State, launchHead, defaultBranch string) {
	dir, final := state.Worktree, state.HeadSHA
	if dir == "" || final == "" {
		return
	}
	if _, err := os.Stat(dir); err != nil {
		return
	}
	var branches []string
	if cur := currentBranch(ctx, dir); cur != "" && cur != state.Branch {
		branches = append(branches, cur)
	}
	if state.Branch != "" {
		branches = append(branches, state.Branch)
	}
	var heads []pushedHead
	for _, b := range branches {
		if b == defaultBranch {
			continue
		}
		sha, err := git.RemoteBranchSHA(ctx, dir, b)
		if err != nil || sha == "" {
			continue
		}
		if !git.IsAncestor(ctx, dir, sha, "refs/heads/"+b) || launchHead != "" && git.IsAncestor(ctx, dir, sha, launchHead) {
			continue
		}
		heads = append(heads, pushedHead{branch: b, sha: sha, changeID: changeIDOf(ctx, dir, sha)})
	}
	if len(heads) == 0 {
		return
	}
	chosen := heads[0]
	if want := changeIDOf(ctx, dir, final); want != "" {
		for _, h := range heads {
			if h.changeID == want {
				chosen = h
				break
			}
		}
	}
	changes := make(map[string]bool)
	for _, h := range heads {
		if h.changeID != "" {
			changes[h.changeID] = true
		}
	}

	launchChange, launchBranch := state.ChangeID, state.Branch
	if chosen.changeID != "" && chosen.changeID != launchChange {
		state.ChangeID = chosen.changeID
		state.TookOver = nil
		if state.Unready != nil && state.Unready.ChangeID == "" {
			state.Unready.ChangeID = launchChange
		}
	}
	state.Branch = chosen.branch
	branch, sha := chosen.branch, chosen.sha
	state.PushedBranch, state.PushedSHA = &branch, &sha
	state.PushedSubject = commitSubject(ctx, dir, sha)

	if state.ChangeID == launchChange && state.Branch == launchBranch && len(changes) < 2 {
		return
	}
	data := map[string]interface{}{
		"id":               state.ID,
		"change_id":        state.ChangeID,
		"branch":           branch,
		"head":             sha,
		"launch_change_id": launchChange,
		"launch_branch":    launchBranch,
	}
	if len(changes) > 1 {
		var pushed []interface{}
		for _, h := range heads {
			pushed = append(pushed, map[string]interface{}{"change_id": h.changeID, "branch": h.branch, "head": h.sha})
		}
		data["pushed"] = pushed
	}
	slog.Info("the run pushed another change or branch than launch recorded", "id", state.ID,
		"change_id", state.ChangeID, "branch", branch, "launch_change_id", launchChange, "launch_branch", launchBranch)
	if baseDir != "" {
		emitRunEvent(baseDir, state, event.AgentChangeRecorded, data)
	}
}

// currentBranch is the branch checked out in dir, or "" for a detached HEAD.
func currentBranch(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// changeIDOf is the change ID commit rev's Change-Id trailer names in dir,
// or "" when it names none in the form klaus gives change IDs.
func changeIDOf(ctx context.Context, dir, rev string) string {
	id, err := git.CommitChangeID(ctx, dir, rev)
	if err != nil || !run.ValidChangeID(id) {
		return ""
	}
	return id
}

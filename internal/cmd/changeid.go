package cmd

import (
	"context"
	"time"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
)

// changeIDInput describes what a new run continues, for chooseChangeID.
type changeIDInput struct {
	IsPRFix       bool
	PRURL         string        // the PR's URL, if known
	PRBranch      string        // the PR's head branch
	TargetRepo    *string       // the run's normalized target repo, as in run.State
	ResumeFrom    string        // --resume-from run ID
	Change        *changeTarget // the change a --change run continues
	Worktree      string        // the new run's worktree; on the PR branch for a --pr run
	DefaultBranch string
}

// changeChoice is the change ID a new run uses, and how it was found.
type changeChoice struct {
	ID        string
	From      string // for the launch output
	Inherited bool   // the run continues an existing change
	Repo      string // the change's repo as the run it was inherited from names it (integration.RepoName); "" when not inherited from a run
}

// chooseChangeID picks the change ID for a new run. A run that continues
// another keeps its change. A --change run takes the change it names, and
// the repo the valley or its runs name it in, whatever --resume-from says. A
// --pr run, budget-paused replays included, takes the ID of the run that
// opened the PR: the earliest recorded run on the PR that has one, in any
// session, else the Change-Id on the PR's head commit when it is not on the
// default branch. A --resume-from run takes the ID of the run it resumes. A
// change inherited from a run keeps that run's repo too, so the new run
// shares its change whatever the valley or its target repo calls the repo.
// Anything else, or a run whose source has none (its state predates change
// IDs), gets a new ID, and From says which.
func chooseChangeID(ctx context.Context, store run.StateStore, in changeIDInput) (changeChoice, error) {
	if c := in.Change; c != nil {
		return changeChoice{ID: c.ID, From: "inherited from change " + c.Name, Inherited: true, Repo: c.Repo}, nil
	}
	var unknown string
	if in.IsPRFix {
		states, _ := run.ListAllSessions()
		if s := prChangeSource(states, in.PRURL, in.PRBranch, in.TargetRepo); s != nil {
			return changeChoice{ID: s.ChangeID, From: "inherited from run " + s.ID, Inherited: true,
				Repo: integration.RepoName(s)}, nil
		}
		// The PR may have been opened on another machine.
		if ahead, err := git.CommitsAhead(ctx, in.Worktree, "origin/"+in.DefaultBranch, "HEAD"); err == nil && ahead > 0 {
			if id, err := git.CommitChangeID(ctx, in.Worktree, "HEAD"); err == nil && run.ValidChangeID(id) {
				return changeChoice{ID: id, From: "inherited from the PR's head commit", Inherited: true}, nil
			}
		}
		unknown = "the PR"
	}
	if in.ResumeFrom != "" {
		if prior, err := store.Load(in.ResumeFrom); err == nil && run.ValidChangeID(prior.ChangeID) {
			return changeChoice{ID: prior.ChangeID, From: "inherited from run " + prior.ID, Inherited: true,
				Repo: integration.RepoName(prior)}, nil
		}
		if unknown == "" {
			unknown = "run " + in.ResumeFrom
		}
	}

	id, err := run.GenChangeID()
	if err != nil {
		return changeChoice{}, err
	}
	c := changeChoice{ID: id, From: "new"}
	if unknown != "" {
		c.From = "new; no change ID is recorded for " + unknown
	}
	return c, nil
}

// prChangeSource returns the earliest-created run on the PR that has a change
// ID, normally the run that opened it, or nil. A run is on the PR when its PR
// URL is prURL, or, when either URL is unknown, its target repo is known and
// is targetRepo, and its branch is branch.
func prChangeSource(states []*run.State, prURL, branch string, targetRepo *string) *run.State {
	var first *run.State
	for _, s := range states {
		if s.Type == "session" || !run.ValidChangeID(s.ChangeID) {
			continue
		}
		if prURL != "" && s.PRURL != nil && *s.PRURL != "" {
			if *s.PRURL != prURL {
				continue
			}
		} else if branch == "" || s.Branch != branch || !sameTarget(s.TargetRepo, targetRepo) {
			continue
		}
		if first == nil || createdBefore(s, first) {
			first = s
		}
	}
	return first
}

// sameTarget reports whether two runs' target repos are known and the same.
func sameTarget(a, b *string) bool {
	return a != nil && b != nil && *a != "" && *a == *b
}

// createdBefore reports whether run a was created before run b. Unparseable
// times sort last.
func createdBefore(a, b *run.State) bool {
	ta, errA := time.Parse(time.RFC3339, a.CreatedAt)
	tb, errB := time.Parse(time.RFC3339, b.CreatedAt)
	if errA != nil || errB != nil {
		return errA == nil
	}
	return ta.Before(tb)
}

// Package integration observes whether the work of a run that finished by
// pushing its branch (no PR) has landed on its repo's default branch.
//
// It is a display-time observation: nothing is persisted and no event is
// emitted. If an observation is wrong, the next check corrects it.
package integration

import (
	"context"

	"github.com/patflynn/klaus/internal/run"
)

// Verdict is what a check observed about one pushed run.
type Verdict int

const (
	// Unknown means the check could not complete (no clone, origin
	// unreachable, fetch failed), or the branch is gone but the clone lacks
	// the history to rule out that it landed. Callers show the run as
	// awaiting.
	Unknown Verdict = iota
	// Awaiting means the branch is on origin and its work has not landed.
	Awaiting
	// Integrated means the pushed SHA, or the branch's current tip, is an
	// ancestor of the default branch.
	Integrated
	// IntegratedContent means a commit on the default branch carries the
	// same change as the branch (equal patch-ids). It is evidence, not proof.
	IntegratedContent
	// Gone means the branch was deleted from origin without landing, as far
	// as a complete check of the clone's history can tell.
	Gone
)

// Landed reports whether the verdict says the run's work is on the default
// branch.
func (v Verdict) Landed() bool {
	return v == Integrated || v == IntegratedContent
}

func (v Verdict) String() string {
	switch v {
	case Awaiting:
		return "awaiting"
	case Integrated:
		return "integrated"
	case IntegratedContent:
		return "integrated-content"
	case Gone:
		return "gone"
	default:
		return "unknown"
	}
}

// Checker observes integration for a set of pushed runs and returns a
// verdict per run ID. Runs it can't judge are Unknown or missing from the
// result; callers treat both as awaiting.
//
// GitChecker is the implementation klaus uses. A provider that knows more
// about a particular origin (for example one that reads `valley status` to
// tell "submitted, waiting on the integrator" from "stale" or "rejected") can
// implement Checker too, deferring to GitChecker for what it doesn't know.
type Checker interface {
	Check(ctx context.Context, runs []*run.State) map[string]Verdict
}

// Applies reports whether a run carries what a check needs: the branch it
// pushed and the SHA it pushed.
func Applies(s *run.State) bool {
	return s.PushedBranch != nil && *s.PushedBranch != "" && s.PushedSHA != nil && *s.PushedSHA != ""
}

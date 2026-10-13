// Package integration observes whether the work of a run that finished by
// pushing its branch (no PR) has landed on its repo's default branch, and,
// for a repo that delivers to a valley, where the run's change stands there.
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

	// The verdicts below come from a valley's record of the run's version
	// (ValleyChecker). A version the valley says landed is Integrated.

	// Ready means the version is its change's active one, ready and
	// awaiting the operator's approval.
	Ready
	// Approved means an integration request is filed for the version and the
	// integrator hasn't decided it yet.
	Approved
	// Uncertain means a request for the version was consumed or failed with
	// no landing evidence, or the valley's record says something klaus
	// doesn't know. The operator must look.
	Uncertain
	// Stale, Rejected and Failed are the integrator's decision on the
	// version's request.
	Stale
	Rejected
	Failed
	// Withdrawn means the change was withdrawn.
	Withdrawn
	// Superseded means another version of the change is the active one.
	Superseded
)

// Landed reports whether the verdict says the run's work is on the default
// branch.
func (v Verdict) Landed() bool {
	return v == Integrated || v == IntegratedContent
}

// Settled reports whether nothing is left to do for the run: its work
// landed, its branch is gone, or its change was withdrawn or superseded.
func (v Verdict) Settled() bool {
	return v.Landed() || v == Gone || v == Withdrawn || v == Superseded
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
	case Ready:
		return "ready"
	case Approved:
		return "approved"
	case Uncertain:
		return "uncertain"
	case Stale:
		return "stale"
	case Rejected:
		return "rejected"
	case Failed:
		return "failed"
	case Withdrawn:
		return "withdrawn"
	case Superseded:
		return "superseded"
	default:
		return "unknown"
	}
}

// Result is what a check observed about one pushed run.
type Result struct {
	Verdict Verdict
	// Change is the valley's record of the run's change when a valley placed
	// the run; nil when git judged it.
	Change *Change
}

// Change is how a valley knows a run: the version of a change that the
// run's pushed head is.
type Change struct {
	Repo     string // the valley's name for the repo
	ChangeID string
	Handle   string
	Title    string
	Version  int    // the run's version
	Head     string // the version's head, which is the run's pushed SHA
}

// Checker observes integration for a set of pushed runs and returns a result
// per run ID. Runs it can't judge are Unknown or missing from the result;
// callers treat both as awaiting.
//
// GitChecker judges runs with git alone. ValleyChecker asks a valley about
// the runs it serves and defers to a GitChecker for the rest.
type Checker interface {
	Check(ctx context.Context, runs []*run.State) map[string]Result
}

// Applies reports whether a run carries what a check needs: the branch it
// pushed and the SHA it pushed.
func Applies(s *run.State) bool {
	return s.PushedBranch != nil && *s.PushedBranch != "" && s.PushedSHA != nil && *s.PushedSHA != ""
}

// RecordedChange is the change version finalize recorded for a run when it
// marked the run's pushed head ready on a valley, or nil when it marked none.
// It stands in for the valley's view when a check couldn't ask the valley.
func RecordedChange(s *run.State) *Change {
	v := s.Valley
	if v == nil || v.Handle == "" || s.PushedSHA == nil {
		return nil
	}
	return &Change{Repo: v.Repo, ChangeID: s.ChangeID, Handle: v.Handle, Title: v.Title, Version: v.Version, Head: *s.PushedSHA}
}

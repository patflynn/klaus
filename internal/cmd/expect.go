package cmd

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// exitNoWorker is launch's exit status, with --run-id, for a launch that
// failed before starting its worker.
const exitNoWorker = 3

// runIDPattern is the form run.GenID gives run IDs.
var runIDPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{4}-[0-9a-f]{8}$`)

// validRunID reports whether id has the form of a run ID launch mints.
func validRunID(id string) bool { return runIDPattern.MatchString(id) }

// changeExpectation is what a launch's --expect-* flags require of the
// change --change names, as the valley lists it: a dispatcher that checked
// the change before launching passes what it checked, and launch starts no
// worker unless the change still stands so. A zero field is not checked.
type changeExpectation struct {
	Owner  string // the change record's owner
	Branch string // the active version's branch, and the launch's
	Head   string // the active version's head, and origin's tip of the branch
	Draft  bool   // the change is a draft
}

// set reports whether any expectation is set.
func (x changeExpectation) set() bool {
	return x.Owner != "" || x.Branch != "" || x.Head != "" || x.Draft
}

// validate rejects expectations launch can't check: they name the change by
// its full ID, with --change, and a head by its full SHA.
func (x changeExpectation) validate(changeRef string) error {
	switch {
	case !x.set():
		return nil
	case !run.ValidChangeID(changeRef):
		return errors.New("--expect-owner, --expect-branch, --expect-head and --expect-draft check the change --change names by its full change ID")
	case x.Head != "" && !valley.IsOID(x.Head):
		return fmt.Errorf("--expect-head %q isn't a full commit SHA", x.Head)
	}
	return nil
}

// check reads the change t through list, the valley's listing of the
// target repo, and returns an error unless the change stands as expected:
// listed, by the ID launch resolved; owned, a draft, and with its active
// version on the branch and at the head expected; and with launch working
// on that branch, whose tip on origin (as fetched) is that head. A valley
// that can't be asked fails the check.
func (x changeExpectation) check(t *changeTarget, list func() (*valley.Changes, error)) error {
	if t == nil {
		return errors.New("expectations need a change to check")
	}
	l, err := list()
	if err != nil {
		return fmt.Errorf("the valley couldn't be asked to confirm change %s: %w", t.Name, err)
	}
	_, ch, v, ok := activeVersionOf(l, t.ID)
	switch {
	case !ok:
		return fmt.Errorf("the valley lists no active version of change %s", t.Name)
	case x.Owner != "" && ch.Owner != x.Owner:
		return fmt.Errorf("change %s's owner is %q, not %q", t.Name, ch.Owner, x.Owner)
	case x.Draft && ch.State != valley.StateDraft:
		return fmt.Errorf("change %s is %s, not a draft", t.Name, ch.State)
	case x.Branch != "" && (v.Branch != x.Branch || t.Branch != x.Branch):
		return fmt.Errorf("change %s's branch is %s, not %s", t.Name, v.Branch, x.Branch)
	case x.Head != "" && v.Head != x.Head:
		return fmt.Errorf("change %s's head is %s, not %s", t.Name, shortSHA(v.Head), shortSHA(x.Head))
	case x.Head != "" && t.Tip != x.Head:
		return fmt.Errorf("origin's %s is at %s, not change %s's expected head %s", t.Branch, shortSHA(t.Tip), t.Name, shortSHA(x.Head))
	}
	return nil
}

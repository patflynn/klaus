// Package restack dispatches resolution runs for the restack conflicts a
// valley reports on changes klaus owns.
//
// A valley's host restacker rebases ready changes onto main after each
// landing. When a rebase conflicts it writes a report beside the change
// record (refs/the-valley/restack/main/<change ID>), and the valley lists
// the latest one with the change, saying whether it is current: whether
// every ref it observed still holds what it observed. This package reads
// those reports and, for a current conflict on a change whose record names
// klaus's principal as its owner, launches a run to resolve it, as the
// pipeline dispatches conflict fixes for GitHub PRs.
//
// A report is an observation, not an instruction. klaus acts on one only
// after its own reads agree with it: before dispatching it reads the change
// and origin's refs again and skips a report whose inputs moved. It runs
// nothing a report supplies, and builds the run's prompt from its own reads
// alone: identities it checked, never the report's paths or text.
package restack

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

const (
	// MaxAttempts is how many resolution runs klaus dispatches for one
	// change before it stops, until a restack of the change succeeds: the
	// pipeline's budget for a PR's conflict fixes.
	MaxAttempts = 3
	// Cooldown is the least time between two dispatches for one change.
	Cooldown = 60 * time.Second
	// LaunchHorizon is how long klaus waits for a launch it started, and
	// lost sight of, to record its run. A pass that started a launch and
	// ended before launch returned leaves the launch pending; once a run by
	// its ID is recorded it is a dispatch, and until then the change is
	// taken to have a run starting. Past the horizon klaus stops waiting,
	// and still counts the attempt: nothing proved no worker started.
	LaunchHorizon = 10 * time.Minute
)

// Key names a change: the valley's name for its repo, and its change ID.
type Key struct{ Repo, ChangeID string }

func (k Key) String() string { return k.Repo + "/" + k.ChangeID }

// Conflict is a current conflict report on a change klaus owns, with the
// change as the same listing shows it.
type Conflict struct {
	Key
	Handle  string
	Target  string // the branch the change lands on, main
	Owner   string // the record's owner: klaus's principal
	Branch  string // the active version's branch, as the record names it
	Version int    // the active version
	Head    string // the active version's head
	Record  string // the change's record commit
	// Report is the report's commit, and TargetTip the target's tip the
	// report observed.
	Report    string
	TargetTip string
	// Held says why klaus leaves the conflict alone, or "" when it may
	// resolve it: the failing layer is another change, or the change sits
	// on others.
	Held string

	layers []layer
}

// layer is a change a report says it rebased together with the conflict's,
// bottom first: the record and branch tip the report observed, and the
// branch the valley's record names for it, which the report's agrees with.
type layer struct {
	ChangeID  string
	Record    string // the record commit the report observed
	Branch    string // the active version's branch, from the change's record
	BranchTip *string
}

// Clean is a current report saying a change klaus owns restacked cleanly.
type Clean struct {
	Key
	Target string
	Report string // the report's commit
}

// Scan reads a repo's listing for the restack reports on changes principal
// owns: owner is the change record's, not any version's uploader, since
// the restacker uploads the versions it rebases. It returns the current
// conflicts on ready changes, and the changes whose current report says
// they restacked cleanly. A report klaus's reading of the listing
// contradicts is skipped as stale, whatever the valley says: its change's
// layer names another record, version, head or branch, or another layer
// names a branch its change's record doesn't, or one the listing lacks.
// A principal of "" owns nothing.
func Scan(repo valley.Repo, principal string) (conflicts []Conflict, clean []Clean) {
	if principal == "" || repo.Err() != "" {
		return nil, nil
	}
	byID := make(map[string]valley.Change, len(repo.Changes))
	for _, ch := range repo.Changes {
		byID[ch.ChangeID] = ch
	}
	for _, ch := range repo.Changes {
		rs := ch.Restack
		if ch.Owner != principal || rs == nil || !rs.Current || rs.Report == nil {
			continue
		}
		rep := rs.Report
		if rep.ChangeID != ch.ChangeID || rep.Target != ch.Target {
			continue
		}
		key := Key{Repo: repo.Repo, ChangeID: ch.ChangeID}
		if rep.Result == valley.RestackComplete {
			clean = append(clean, Clean{Key: key, Target: ch.Target, Report: rs.Commit})
			continue
		}
		if rep.Result != valley.RestackConflict || rep.Failing == nil || ch.State != valley.StateReady {
			continue
		}
		active, ok := activeVersion(ch)
		own, found := rep.Layer(ch.ChangeID)
		tip := valley.ReportOID(rep.TargetTip)
		if !ok || !found || tip == "" || own.Version != ch.Active ||
			valley.ReportOID(own.Head) != active.Head || valley.ReportOID(own.Record) != ch.Record {
			continue
		}
		layers, agree := checkedLayers(rep.Layers, byID)
		if !agree {
			continue
		}
		c := Conflict{
			Key: key, Handle: ch.Handle, Target: ch.Target, Owner: ch.Owner, Branch: active.Branch,
			Version: ch.Active, Head: active.Head, Record: ch.Record,
			Report: rs.Commit, TargetTip: tip, layers: layers,
		}
		c.Held = held(c, *rep.Failing, ch, byID, principal)
		conflicts = append(conflicts, c)
	}
	return conflicts, clean
}

// checkedLayers returns a report's layers with the branch each change's
// record names, and whether every layer's agrees with it: a branch the
// report names, or leaves out, is never the one klaus checks for movement
// unless the record names it too. A layer whose change the listing lacks,
// or whose record names no branch, can't be checked.
func checkedLayers(layers []valley.RestackLayer, byID map[string]valley.Change) ([]layer, bool) {
	out := make([]layer, 0, len(layers))
	for _, l := range layers {
		ch, listed := byID[l.ChangeID]
		v, ok := activeVersion(ch)
		if !listed || !ok || v.Branch == "" || l.Branch != v.Branch {
			return nil, false
		}
		out = append(out, layer{ChangeID: l.ChangeID, Record: valley.ReportOID(l.Record), Branch: v.Branch, BranchTip: l.BranchTip})
	}
	return out, true
}

// held says why klaus leaves a conflict alone, or "" when it may resolve
// it. It resolves a conflict only in the change itself, at the bottom of
// what the restacker rebased: a conflict in a change below belongs to that
// change, and klaus never takes over another owner's change; and a change
// that sits on other changes can't be rebased onto main on its own.
func held(c Conflict, failing string, ch valley.Change, byID map[string]valley.Change, principal string) string {
	if failing != c.ChangeID {
		name := failing
		f, listed := byID[failing]
		if listed && f.Handle != "" {
			name = f.Handle
		}
		if listed && f.Owner == principal {
			return "the conflict is in " + name + ", a change below it, which is resolved first"
		}
		return "the conflict is in " + name + ", a change below it that klaus doesn't own"
	}
	if len(c.layers) > 0 && c.layers[0].ChangeID != c.ChangeID || len(ch.Includes) > 0 {
		return "it sits on other changes; klaus resolves a conflict only in a change that sits on main"
	}
	return ""
}

// activeVersion returns a change's active version.
func activeVersion(ch valley.Change) (valley.Version, bool) {
	for _, v := range ch.Versions {
		if v.N == ch.Active {
			return v, true
		}
	}
	return valley.Version{}, false
}

// Decision is what a pass does about a conflict.
type Decision int

const (
	// Dispatch launches a resolution run.
	Dispatch Decision = iota
	// Held leaves a conflict klaus doesn't resolve (Conflict.Held).
	Held
	// Acted leaves a conflict klaus already acted on: an earlier pass
	// dispatched for, or refused, the same head and target tip.
	Acted
	// Busy leaves a change a run is working on, or one klaus is launching
	// a run for.
	Busy
	// Stop leaves a change whose attempts are spent.
	Stop
	// Wait leaves a change dispatched for within the cooldown.
	Wait
)

// Decide is what a pass does about conflict c at now, given the ledger's
// entry for its change (nil for none) and whether a run is working on the
// change. As the pipeline does for a PR's conflicts, it never starts a
// second run on a change, nor while a launch klaus started is pending;
// stops once MaxAttempts runs were dispatched, and waits Cooldown after
// each launch that may have started one; and it never acts twice on one
// set of inputs, the change's head and the target's tip, whichever came
// between, so an unchanged conflict isn't retried. Spent attempts come
// before that once no run is working on the change, so a last run that
// left the conflict as it was is seen to have stopped.
func Decide(now time.Time, c Conflict, e *Entry, running bool) Decision {
	spent := e != nil && e.Attempts >= MaxAttempts
	switch {
	case c.Held != "":
		return Held
	case e != nil && e.Last != nil && e.Last.Pending:
		return Busy
	case e.consumed(Inputs{Head: c.Head, TargetTip: c.TargetTip}) && (running || !spent):
		return Acted
	case running:
		return Busy
	case spent:
		return Stop
	case e != nil && e.Last != nil && e.Last.RunID != "" && now.Sub(e.Last.At) < Cooldown:
		return Wait
	}
	return Dispatch
}

// Stale compares a conflict's report with origin's refs as klaus read them
// itself (refs, by name, as OriginRefs returns them), and says which input
// moved, or "" when every one still holds: the target's tip, the report
// itself, and for each layer its record and the tip (or the absence) of
// the branch its record names.
func Stale(c Conflict, refs map[string]string) string {
	switch {
	case refs["refs/heads/"+c.Target] != c.TargetTip:
		return c.Target + " moved since the report"
	case refs[ReportRef(c.Target, c.ChangeID)] != c.Report:
		return "a newer report replaced it"
	}
	for _, l := range c.layers {
		if refs[RecordRef(c.Target, l.ChangeID)] != l.Record {
			return "the record of " + l.ChangeID + " moved since the report"
		}
		tip, present := refs["refs/heads/"+l.Branch]
		switch {
		case l.BranchTip == nil && present:
			return "branch " + l.Branch + " appeared since the report"
		case l.BranchTip != nil && tip != valley.ReportOID(*l.BranchTip):
			return "branch " + l.Branch + " moved since the report"
		}
	}
	return ""
}

// Refs are the refs Stale compares.
func Refs(c Conflict) []string {
	refs := []string{"refs/heads/" + c.Target, ReportRef(c.Target, c.ChangeID)}
	for _, l := range c.layers {
		refs = append(refs, RecordRef(c.Target, l.ChangeID), "refs/heads/"+l.Branch)
	}
	return refs
}

// RecordRef is the ref of a change's record on origin.
func RecordRef(target, id string) string { return "refs/the-valley/changes/" + target + "/" + id }

// ReportRef is the ref of a change's restack report on origin.
func ReportRef(target, id string) string { return "refs/the-valley/restack/" + target + "/" + id }

// plainName matches a name safe to put in a prompt as it is: a repo,
// branch or handle with no spaces, quotes or control characters.
var plainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

// Prompt is the brief of a run resolving conflict c. It holds only what
// klaus read and checked itself: the change's ID, the version, head and
// branch the valley's record names, and the target's tip origin holds,
// which the report matched. None of the report's free text, such as its
// conflict paths, goes in: the run finds the conflicts by rebasing.
func Prompt(c Conflict) string {
	if !run.ValidChangeID(c.ChangeID) || !valley.IsOID(c.Head) || !valley.IsOID(c.TargetTip) {
		return ""
	}
	name := c.ChangeID
	if plainName.MatchString(c.Handle) {
		name = c.Handle + " (" + c.ChangeID + ")"
	}
	repo, target := "its repo", "main"
	if plainName.MatchString(c.Repo) {
		repo = c.Repo
	}
	if plainName.MatchString(c.Target) {
		target = c.Target
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The valley's restacker could not rebase change %s of %s onto %s: the rebase conflicts in this change's own commits.\n\n", name, repo, target)
	b.WriteString("klaus read, before launching this run:\n")
	fmt.Fprintf(&b, "- the change: version %d, head %s", c.Version, c.Head)
	if plainName.MatchString(c.Branch) {
		fmt.Fprintf(&b, ", branch %s", c.Branch)
	}
	fmt.Fprintf(&b, "\n- %s on origin: %s\n\n", target, c.TargetTip)
	fmt.Fprintf(&b, "Rebase the change onto origin/%s and resolve the conflicts, keeping what both sides meant. Find the conflicting paths by running the rebase; nothing else names them for you. ", target)
	b.WriteString("Run the tests, then push the rebased head with the leased push your instructions give. ")
	b.WriteString("Rewrite only this change's commits: if a conflict lies in another change's commits, stop and say so. ")
	b.WriteString("The change is a draft while you work; finalize marks the head you push ready again.\n")
	return b.String()
}

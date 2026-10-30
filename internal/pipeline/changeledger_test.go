package pipeline

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/valley"
)

// TestChangeLedgerSharedByTwoDashboards has two dashboards on one session,
// each with its own ChangeLedger on the session's file, loaded before
// either saw the change, take turns observing it. Each transition is
// reported by whichever sees it first, and never by the other.
func TestChangeLedgerSharedByTwoDashboards(t *testing.T) {
	path := filepath.Join(t.TempDir(), ChangeLedgerFile)
	a, err := LoadChangeLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadChangeLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	observe := observer(t)
	v1 := valley.Version{N: 1, Head: "h1", Landing: valley.NotLanded}
	v2 := valley.Version{N: 2, Head: "h2", Landing: valley.NotLanded, Uploader: "restacker"}

	observe("a's first look", a, change(v1))
	observe("b's first look, after a's", b, change(v1))

	requested := v1
	requested.Request = &valley.Request{Ref: "refs/valley/requests/nxvp"}
	observe("a sees the approval", a, change(requested), event.ChangeApproved)
	observe("b sees it after a", b, change(requested))

	stale := requested
	stale.Outcome = &valley.Outcome{Decision: "stale"}
	observe("b sees it go stale", b, change(stale), event.ChangeStale)
	observe("a sees it after b", a, change(stale))

	observe("a sees the restacker's version", a, change(stale, v2), event.ChangeNewVersion)
	observe("b sees it after a", b, change(stale, v2))

	rejected := v2
	rejected.Request, rejected.Outcome = &valley.Request{Ref: "refs/valley/requests/nxvp"}, &valley.Outcome{Decision: "rejected"}
	observe("a sees v2 approved and rejected", a, change(stale, rejected), event.ChangeApproved, event.ChangeRejected)
	observe("b, with a listing read before a's, sees v1 stale", b, change(stale))

	// A third dashboard, started now, reads what the two recorded.
	c, err := LoadChangeLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	observe("a restarted dashboard", c, change(stale, rejected))
}

// TestChangeLedgerOlderListing has two dashboards on one session, where
// the second holds a listing read before the one the first recorded a
// later version from. What the older listing says of the superseded
// version's request is old news, and reports nothing; the first landing
// either listing shows is the change's only one.
func TestChangeLedgerOlderListing(t *testing.T) {
	observe := observer(t)
	path := filepath.Join(t.TempDir(), ChangeLedgerFile)
	a, err := LoadChangeLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadChangeLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	v1 := valley.Version{N: 1, Head: "h1", Landing: valley.NotLanded}
	v2 := valley.Version{N: 2, Head: "h2", Landing: valley.NotLanded, Uploader: "restacker"}
	with := func(v valley.Version, decision string) valley.Version {
		v.Request, v.Outcome = &valley.Request{Ref: "refs/valley/requests/nxvp"}, &valley.Outcome{Decision: decision}
		return v
	}
	requested := v1
	requested.Request = &valley.Request{Ref: "refs/valley/requests/nxvp"}

	observe("a's first look", a, change(v1))
	observe("a sees the restacker's version", a, change(v1, v2), event.ChangeNewVersion)
	observe("b, with a listing where v1 is approved", b, change(requested))
	observe("b, with a listing where v1 went stale", b, change(with(v1, "stale")))
	observe("b, with a listing where v1 was rejected", b, change(with(v1, "rejected")))
	withdrawn := change(requested)
	withdrawn.State = valley.StateWithdrawn
	observe("b, with a listing where the change was withdrawn at v1", b, withdrawn)
	observe("a sees v2 approved and stale", a, change(v1, with(v2, "stale")), event.ChangeApproved, event.ChangeStale)
	observe("b, with a listing where v1 failed", b, change(with(v1, "failed")))

	// A landing is news from any listing, once.
	landed := v1
	landed.Landing = valley.Landed
	observe("b, with a listing where v1 landed", b, change(landed), event.ChangeIntegrated)
	observe("a sees v1 landed under v2", a, change(landed, with(v2, "stale")))

	l, err := LoadChangeLedger(filepath.Join(t.TempDir(), ChangeLedgerFile))
	if err != nil {
		t.Fatal(err)
	}
	observe("a first look", l, change(v1))
	observe("a newer listing shows v1 landed under v2", l, change(landed, v2), event.ChangeNewVersion, event.ChangeIntegrated)
	observe("an older listing shows v1 landed", l, change(landed))
}

// observer returns a function that has l observe ch, as the change nxvp in
// the valley's repo cosmo, whose version 1 this session recorded, and
// checks the transitions it reports, at step.
func observer(t *testing.T) func(step string, l *ChangeLedger, ch valley.Change, want ...string) {
	own := func(v valley.Version) bool { return v.N == 1 }
	return func(step string, l *ChangeLedger, ch valley.Change, want ...string) {
		t.Helper()
		ts, err := l.Observe("cosmo", ch, own)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, tr := range ts {
			got = append(got, tr.Type)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: transitions %q, want %q", step, got, want)
		}
	}
}

// change is the ready change nxvp with versions vs, the last active.
func change(vs ...valley.Version) valley.Change {
	return valley.Change{ChangeID: "nxvp-id", Handle: "nxvp", State: valley.StateReady, Active: vs[len(vs)-1].N, Versions: vs}
}

// TestChangeLedgerLandingBeforeANewVersion: a version that landed is the
// change's landing even when another version became active before the
// ledger saw it land.
func TestChangeLedgerLandingBeforeANewVersion(t *testing.T) {
	l, err := LoadChangeLedger(filepath.Join(t.TempDir(), ChangeLedgerFile))
	if err != nil {
		t.Fatal(err)
	}
	own := func(valley.Version) bool { return false }
	commit := "c1"
	v1 := valley.Version{N: 1, Head: "h1", Landing: valley.NotLanded, Request: &valley.Request{Ref: "r"}}
	if _, err := l.Observe("cosmo", valley.Change{ChangeID: "x", State: valley.StateReady, Active: 1, Versions: []valley.Version{v1}}, own); err != nil {
		t.Fatal(err)
	}
	v1.Landing, v1.LandedCommit = valley.Landed, &commit
	v2 := valley.Version{N: 2, Head: "h2", Landing: valley.NotLanded}
	ts, err := l.Observe("cosmo", valley.Change{ChangeID: "x", State: valley.StateReady, Active: 2, Versions: []valley.Version{v1, v2}}, own)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tr := range ts {
		got = append(got, fmt.Sprintf("%s v%d %s", tr.Type, tr.Version.N, tr.Commit))
	}
	if want := []string{event.ChangeNewVersion + " v2 ", event.ChangeIntegrated + " v1 c1"}; !slices.Equal(got, want) {
		t.Errorf("transitions %q, want %q", got, want)
	}
}

package pipeline

import (
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
	own := func(v valley.Version) bool { return v.N == 1 }
	v1 := valley.Version{N: 1, Head: "h1", Landing: valley.NotLanded}
	v2 := valley.Version{N: 2, Head: "h2", Landing: valley.NotLanded, Uploader: "restacker"}
	change := func(vs ...valley.Version) valley.Change {
		return valley.Change{ChangeID: "nxvp-id", Handle: "nxvp", State: valley.StateReady, Active: vs[len(vs)-1].N, Versions: vs}
	}
	observe := func(step string, l *ChangeLedger, ch valley.Change, want ...string) {
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

	// A third dashboard, started now, reads what the two recorded.
	c, err := LoadChangeLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	observe("a restarted dashboard", c, change(stale, v2))
}

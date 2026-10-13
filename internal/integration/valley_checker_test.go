package integration

import (
	"testing"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// changesFixture is a valley-changes/v1 listing with a change in every state
// klaus reads, in two repos that share a change ID.
const changesFixture = `{
  "schema": "valley-changes/v1",
  "host": "classic-laddie",
  "repos": [
    { "repo": "cosmo", "url": "git@classic-laddie:cosmo.git", "error": null,
      "changes": [
        { "change_id": "nxvp-id", "handle": "nxvp", "target": "main", "state": "ready", "active": 2,
          "versions": [
            { "n": 1, "head": "nxvp-1", "title": "First try", "landing": "not-landed" },
            { "n": 2, "head": "nxvp-2", "title": "Second try", "landing": "not-landed" }
          ] },
        { "change_id": "appr-id", "handle": "appr", "state": "ready", "active": 1,
          "versions": [ { "n": 1, "head": "appr-1", "landing": "not-landed",
            "request": { "ref": "refs/valley/requests/appr", "commit": "r" }, "outcome": null } ] },
        { "change_id": "land-id", "handle": "land", "state": "ready", "active": 2,
          "versions": [
            { "n": 1, "head": "land-1", "landing": "landed" },
            { "n": 2, "head": "land-2", "landing": "landed-recorded" }
          ] },
        { "change_id": "with-id", "handle": "with", "state": "withdrawn", "active": 1,
          "versions": [ { "n": 1, "head": "with-1", "landing": "not-landed" } ] },
        { "change_id": "unce-id", "handle": "unce", "state": "ready", "active": 1,
          "versions": [ { "n": 1, "head": "unce-1", "landing": "uncertain",
            "request": { "ref": "x", "commit": "r" } } ] },
        { "change_id": "stal-id", "handle": "stal", "state": "ready", "active": 3,
          "versions": [
            { "n": 1, "head": "stal-1", "landing": "not-landed", "outcome": { "decision": "stale" } },
            { "n": 2, "head": "stal-2", "landing": "not-landed", "outcome": { "decision": "rejected" } },
            { "n": 3, "head": "stal-3", "landing": "not-landed", "outcome": { "decision": "failed" } }
          ] },
        { "change_id": "redo-id", "handle": "redo", "state": "ready", "active": 2,
          "versions": [
            { "n": 1, "head": "redo-h", "landing": "not-landed" },
            { "n": 2, "head": "redo-h", "landing": "not-landed" }
          ] }
      ] },
    { "repo": "fork", "url": "git@classic-laddie:fork.git", "error": null,
      "changes": [
        { "change_id": "nxvp-id", "handle": "nxvpf", "state": "withdrawn", "active": 1,
          "versions": [ { "n": 1, "head": "nxvp-2", "landing": "not-landed" } ] }
      ] },
    { "repo": "dark", "url": "git@classic-laddie:dark.git", "error": "unreadable", "changes": [] }
  ]
}`

func TestPlaceMatchesRunsToVersions(t *testing.T) {
	changes, err := valley.Decode([]byte(changesFixture))
	if err != nil {
		t.Fatal(err)
	}
	pushed := func(repo, changeID, head string) *run.State {
		branch := "agent/x"
		s := &run.State{ID: changeID + "@" + head, ChangeID: changeID, PushedBranch: &branch, PushedSHA: &head}
		if repo != "" {
			s.Valley = &run.Valley{Repo: repo}
		}
		return s
	}
	target := func(targetRepo string, s *run.State) *run.State {
		s.TargetRepo = &targetRepo
		return s
	}

	for _, tc := range []struct {
		name    string
		run     *run.State
		want    Verdict
		handle  string
		version int
	}{
		{"active version, ready", pushed("cosmo", "nxvp-id", "nxvp-2"), Ready, "nxvp", 2},
		{"earlier version", pushed("cosmo", "nxvp-id", "nxvp-1"), Superseded, "nxvp", 1},
		{"same change in another repo", pushed("fork", "nxvp-id", "nxvp-2"), Withdrawn, "nxvpf", 1},
		{"repo from the target repo's name", target("patflynn/cosmo.git", pushed("", "nxvp-id", "nxvp-2")), Ready, "nxvp", 2},
		{"request filed", pushed("cosmo", "appr-id", "appr-1"), Approved, "appr", 1},
		{"landed", pushed("cosmo", "land-id", "land-2"), Integrated, "land", 2},
		{"landed, then superseded", pushed("cosmo", "land-id", "land-1"), Integrated, "land", 1},
		{"withdrawn", pushed("cosmo", "with-id", "with-1"), Withdrawn, "with", 1},
		{"uncertain", pushed("cosmo", "unce-id", "unce-1"), Uncertain, "unce", 1},
		{"stale, superseded", pushed("cosmo", "stal-id", "stal-1"), Superseded, "stal", 1},
		{"failed", pushed("cosmo", "stal-id", "stal-3"), Failed, "stal", 3},
		{"head readied twice", pushed("cosmo", "redo-id", "redo-h"), Ready, "redo", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := place(changes, tc.run)
			if !ok {
				t.Fatal("run not placed")
			}
			if got.Verdict != tc.want || got.Change.Handle != tc.handle || got.Change.Version != tc.version ||
				got.Change.Head != *tc.run.PushedSHA || got.Change.ChangeID != tc.run.ChangeID {
				t.Errorf("placed as %v %+v, want %v %s v%d", got.Verdict, *got.Change, tc.want, tc.handle, tc.version)
			}
		})
	}

	// The outcome decides: each of stale, rejected and failed, on the
	// active version.
	for decision, want := range map[string]Verdict{"stale": Stale, "rejected": Rejected, "failed": Failed, "deferred": Uncertain} {
		ch := changes.Repos[0].Changes[5]
		v := ch.Versions[2]
		v.Outcome = &valley.Outcome{Decision: decision}
		if got := verdictOf(ch, v); got != want {
			t.Errorf("outcome %q: verdict %v, want %v", decision, got, want)
		}
	}

	for _, tc := range []struct {
		name string
		run  *run.State
	}{
		{"head that isn't a version", pushed("cosmo", "nxvp-id", "elsewhere")},
		{"change the valley lacks", pushed("cosmo", "none-id", "nxvp-2")},
		{"repo the valley lacks", pushed("other", "nxvp-id", "nxvp-2")},
		{"repo the valley couldn't read", pushed("dark", "nxvp-id", "nxvp-2")},
		{"run without a change ID", pushed("cosmo", "", "nxvp-2")},
	} {
		if got, ok := place(changes, tc.run); ok {
			t.Errorf("%s: placed as %v %+v, want unplaced", tc.name, got.Verdict, *got.Change)
		}
	}
}

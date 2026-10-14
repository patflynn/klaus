package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// TestResolveChange covers the order --change resolves a ref in: a run ID,
// then the runs in the target repo by change ID, ID prefix or recorded
// handle, then the valley's listing; and where the branch comes from.
func TestResolveChange(t *testing.T) {
	str := func(s string) *string { return &s }
	id := func(prefix string) string { return prefix + strings.Repeat("k", 32-len(prefix)) }
	pushed := func(s *run.State, branch, sha string) *run.State {
		s.PushedBranch, s.PushedSHA = &branch, &sha
		return s
	}

	// Change nxvp in widget: v1 by an older run, a follow-up v2 recorded in
	// another session, and a run that never pushed.
	v1 := pushed(&run.State{ID: "20261013-0900-0001", TargetRepo: str("cosmo/widget"), ChangeID: id("nxvp"),
		CreatedAt: "2026-10-13T09:00:00Z", Valley: &run.Valley{Repo: "widget", Handle: "nxvp", Title: "Add a health check"}},
		"add-a-health-check-0001", "1111111")
	v2 := pushed(&run.State{ID: "20261013-1000-0002", TargetRepo: str("widget"), ChangeID: id("nxvp"),
		CreatedAt: "2026-10-13T10:00:00Z", Valley: &run.Valley{Repo: "widget", Handle: "nxvp"}},
		"add-a-health-check-0001", "2222222")
	unpushed := &run.State{ID: "20261013-1100-0003", TargetRepo: str("widget"), ChangeID: id("nxvp"), CreatedAt: "2026-10-13T11:00:00Z"}
	// The same handle's change in another repo, and a change whose recorded
	// handle has since lengthened: the listing would now call it rstuv.
	gadget := pushed(&run.State{ID: "20261013-0900-0004", TargetRepo: str("gadget"), ChangeID: id("nxvpz"), Prompt: "port the health check\nto gadget",
		CreatedAt: "2026-10-13T09:00:00Z", Valley: &run.Valley{Repo: "gadget", Handle: "nxvp"}}, "port-0004", "4444444")
	rstu := pushed(&run.State{ID: "20261013-0900-0005", TargetRepo: str("widget"), ChangeID: id("rstuv"),
		CreatedAt: "2026-10-13T09:00:00Z", Valley: &run.Valley{Repo: "widget", Handle: "rstu"}}, "tune-0005", "5555555")
	// A run from before change IDs, one whose valley calls its repo widget
	// while it targeted acme/source, and a second widget change starting nx.
	legacy := &run.State{ID: "20261001-0900-0006", TargetRepo: str("widget"), CreatedAt: "2026-10-01T09:00:00Z"}
	renamed := pushed(&run.State{ID: "20261013-0900-0007", TargetRepo: str("acme/source"), ChangeID: id("wxyz"),
		CreatedAt: "2026-10-13T09:00:00Z", Valley: &run.Valley{Repo: "widget", Handle: "wxyz"}}, "renamed-0007", "7777777")
	nxwr := pushed(&run.State{ID: "20261013-0900-0008", TargetRepo: str("widget"), ChangeID: id("nxwr"), Prompt: "cache the health check\nfor a minute",
		CreatedAt: "2026-10-13T09:00:00Z"}, "cache-0008", "8888888")
	runs := []*run.State{v1, v2, unpushed, gadget, rstu, legacy, renamed, nxwr, {ID: "session-x", Type: "session"}}

	listing := func(changes ...valley.Change) func() (*valley.Changes, error) {
		return func() (*valley.Changes, error) {
			return &valley.Changes{Schema: valley.SchemaV1, Repos: []valley.Repo{{Repo: "widget", Changes: changes}}}, nil
		}
	}
	elsewhere := valley.Change{ChangeID: id("lmno"), Handle: "lmno", State: valley.StateReady, Active: 2, Versions: []valley.Version{
		{N: 1, Head: "aaaaaaa", Branch: "first-try"},
		{N: 2, Head: "bbbbbbb", Branch: "second-try"},
	}}
	unreachable := func() (*valley.Changes, error) { return nil, errors.New("valley changes: cannot reach classic-laddie") }

	for _, tc := range []struct {
		name, ref, repo string
		valley          func() (*valley.Changes, error)
		want            changeTarget // Runs compared by ID
		runIDs          []string
		err             []string
	}{
		{
			name: "a run ID names its run's change, and the newest pushed run gives the branch",
			ref:  v1.ID, repo: "widget",
			want:   changeTarget{ID: id("nxvp"), Name: "nxvp", Repo: "widget", Branch: "add-a-health-check-0001", Head: "2222222"},
			runIDs: []string{unpushed.ID, v2.ID, v1.ID},
		},
		{
			name: "a handle matches the runs in the target repo only",
			ref:  "nxvp", repo: "widget",
			want:   changeTarget{ID: id("nxvp"), Name: "nxvp", Repo: "widget", Branch: "add-a-health-check-0001", Head: "2222222"},
			runIDs: []string{unpushed.ID, v2.ID, v1.ID},
		},
		{
			name: "a full change ID matches its runs",
			ref:  id("nxvpz"), repo: "gadget",
			want:   changeTarget{ID: id("nxvpz"), Name: "nxvp", Repo: "gadget", Branch: "port-0004", Head: "4444444"},
			runIDs: []string{gadget.ID},
		},
		{
			name: "a lengthened handle matches the change ID's prefix",
			ref:  "rstuv", repo: "widget",
			want:   changeTarget{ID: id("rstuv"), Name: "rstu", Repo: "widget", Branch: "tune-0005", Head: "5555555"},
			runIDs: []string{rstu.ID},
		},
		{
			name: "a run is in the repo its valley names, not its target's",
			ref:  "wxyz", repo: "widget",
			want:   changeTarget{ID: id("wxyz"), Name: "wxyz", Repo: "widget", Branch: "renamed-0007", Head: "7777777"},
			runIDs: []string{renamed.ID},
		},
		{
			name: "a handle doesn't name a change in its target's repo when its valley names another",
			ref:  "wxyz", repo: "source",
			err: []string{`no run on a change "wxyz" is recorded in source`},
		},
		{
			name: "a run ID doesn't name a change in its target's repo when its valley names another",
			ref:  renamed.ID, repo: "source",
			err: []string{"run " + renamed.ID + " is in widget, not source"},
		},
		{
			name: "an unknown target repo matches no repo",
			ref:  v1.ID, repo: "",
			err: []string{"can't tell which repo the launch targets"},
		},
		{
			name: "the valley's active version gives the branch and handle when it lists the change",
			ref:  v1.ID, repo: "widget",
			valley: listing(valley.Change{ChangeID: id("nxvp"), Handle: "nxvpk", State: valley.StateReady, Active: 3,
				Versions: []valley.Version{{N: 3, Head: "3333333", Branch: "readied-elsewhere"}}}),
			want:   changeTarget{ID: id("nxvp"), Name: "nxvpk", Repo: "widget", Branch: "readied-elsewhere", Head: "3333333"},
			runIDs: []string{unpushed.ID, v2.ID, v1.ID},
		},
		{
			name: "runs decide when the valley can't be asked",
			ref:  "nxvp", repo: "widget", valley: unreachable,
			want:   changeTarget{ID: id("nxvp"), Name: "nxvp", Repo: "widget", Branch: "add-a-health-check-0001", Head: "2222222"},
			runIDs: []string{unpushed.ID, v2.ID, v1.ID},
		},
		{
			name: "with no run on it, the valley's listing names the change",
			ref:  "lmno", repo: "widget", valley: listing(elsewhere),
			want: changeTarget{ID: id("lmno"), Name: "lmno", Repo: "widget", Branch: "second-try", Head: "bbbbbbb"},
		},
		{
			name: "a handle naming two changes lists them",
			ref:  "nx", repo: "widget",
			err: []string{`"nx" names 2 changes in widget`, id("nxvp") + " (nxvp), latest run " + unpushed.ID + "\n",
				id("nxwr") + ", latest run " + nxwr.ID + ": cache the health check"},
		},
		{
			name: "a handle naming two listed changes lists them",
			ref:  "lmn", repo: "widget",
			valley: listing(elsewhere, valley.Change{ChangeID: id("lmnz"), Handle: "lmnz"}),
			err:    []string{`"lmn" names 2 changes in widget`, id("lmno") + " (lmno)", id("lmnz") + " (lmnz)"},
		},
		{
			name: "a run in another repo is refused",
			ref:  gadget.ID, repo: "widget",
			err: []string{"run " + gadget.ID + " is in gadget, not widget"},
		},
		{
			name: "a run from before change IDs is refused",
			ref:  legacy.ID, repo: "widget",
			err: []string{"run " + legacy.ID + " has no change ID", "--resume-from " + legacy.ID},
		},
		{
			name: "an unknown handle without a valley",
			ref:  "qqqq", repo: "widget",
			err: []string{`no run on a change "qqqq" is recorded in widget`},
		},
		{
			name: "an unknown handle the valley doesn't list either",
			ref:  "qqqq", repo: "widget", valley: listing(elsewhere),
			err: []string{`no change "qqqq": no run in widget records it, and the valley doesn't list it`},
		},
		{
			name: "an unknown handle when the valley can't be asked",
			ref:  "qqqq", repo: "widget", valley: unreachable,
			err: []string{"the valley couldn't be asked", "cannot reach classic-laddie"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveChange(tc.ref, changeSources{Runs: runs, Repo: tc.repo, Valley: tc.valley})
			if tc.err != nil {
				if err == nil {
					t.Fatalf("got %+v, want an error", got)
				}
				for _, want := range tc.err {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q should contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, s := range got.Runs {
				ids = append(ids, s.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.runIDs, ",") {
				t.Errorf("runs %v, want %v newest first", ids, tc.runIDs)
			}
			got.Runs = nil
			if fmt.Sprint(*got) != fmt.Sprint(tc.want) {
				t.Errorf("got %+v, want %+v", *got, tc.want)
			}
		})
	}

	t.Run("the valley is asked once", func(t *testing.T) {
		calls := 0
		_, err := resolveChange("lmno", changeSources{Runs: runs, Repo: "widget", Valley: func() (*valley.Changes, error) {
			calls++
			return listing(elsewhere)()
		}})
		if err != nil || calls != 1 {
			t.Errorf("err %v, valley asked %d times, want once", err, calls)
		}
	})
}

// TestLiveHolder: a continuing launch yields to a live run on its branch or
// its change in the target repo, whatever else that run is on.
func TestLiveHolder(t *testing.T) {
	str := func(s string) *string { return &s }
	change := "nxvp" + strings.Repeat("k", 28)
	onBranch := &run.State{ID: "on-branch", TargetRepo: str("acme/widget"), Branch: "fix-0001"}
	onChange := &run.State{ID: "on-change", TargetRepo: str("widget"), Branch: "other-0002", ChangeID: change}
	valleyNamed := &run.State{ID: "valley-named", TargetRepo: str("acme/source"), Branch: "fix-0001", Valley: &run.Valley{Repo: "widget"}}
	elsewhere := &run.State{ID: "elsewhere", TargetRepo: str("gadget"), Branch: "fix-0001", ChangeID: change}
	for _, tc := range []struct {
		name     string
		states   []*run.State
		live     []string
		branch   string
		changeID string
		repos    []string
		want     string
	}{
		{"a live run on the branch", []*run.State{onBranch}, []string{"on-branch"}, "fix-0001", "", []string{"widget"}, "on-branch"},
		{"a run on the branch that isn't live", []*run.State{onBranch}, nil, "fix-0001", "", []string{"widget"}, ""},
		{"a live run on the change on another branch", []*run.State{onChange}, []string{"on-change"}, "fix-0001", change, []string{"widget"}, "on-change"},
		{"no change ID matches no change", []*run.State{onChange}, []string{"on-change"}, "fix-0001", "", []string{"widget"}, ""},
		{"a run in the repo the change's valley names", []*run.State{valleyNamed}, []string{"valley-named"}, "fix-0001", "", []string{"source", "widget"}, "valley-named"},
		{"a run in another repo", []*run.State{elsewhere}, []string{"elsewhere"}, "fix-0001", change, []string{"widget", ""}, ""},
		{"a session", []*run.State{{ID: "s", Type: "session", TargetRepo: str("widget"), Branch: "fix-0001"}}, []string{"s"}, "fix-0001", "", []string{"widget"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			running := func(s *run.State) bool { return slices.Contains(tc.live, s.ID) }
			var got string
			if s := liveHolder(tc.states, running, tc.branch, tc.changeID, tc.repos...); s != nil {
				got = s.ID
			}
			if got != tc.want {
				t.Errorf("liveHolder = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckContinuation(t *testing.T) {
	for _, tc := range []struct {
		pr, change string
		branch     bool
		err        string
	}{
		{pr: "42"},
		{change: "nxvp"},
		{branch: true},
		{pr: "42", change: "nxvp", err: "--pr and --change are mutually exclusive"},
		{pr: "42", branch: true, err: "--branch and --pr are mutually exclusive"},
		{change: "nxvp", branch: true, err: "--branch and --change are mutually exclusive"},
	} {
		err := checkContinuation(tc.pr, tc.change, tc.branch)
		if (err == nil) != (tc.err == "") || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("checkContinuation(%q, %q, %v) = %v, want %q", tc.pr, tc.change, tc.branch, err, tc.err)
		}
	}
}

// TestChangeBranchMustBeOnOrigin checks the branch against origin as fetched
// into a real clone.
func TestChangeBranchMustBeOnOrigin(t *testing.T) {
	_, repo, _, _ := setupBareRemote(t)
	ctx := context.Background()
	if err := (&changeTarget{Name: "nxvp", Branch: "main", Head: "1111111"}).checkBranch(ctx, repo); err != nil {
		t.Errorf("a branch on origin: %v", err)
	}
	for _, tc := range []struct {
		t    changeTarget
		want string
	}{
		{changeTarget{Name: "nxvp", Branch: "gone", Head: "1111111"}, "change nxvp's branch gone is not on origin (last known head 1111111)"},
		{changeTarget{Name: "nxvp"}, "change nxvp has no known branch or head"},
	} {
		if err := tc.t.checkBranch(ctx, repo); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("checkBranch(%+v) = %v, want %q", tc.t, err, tc.want)
		}
	}
}

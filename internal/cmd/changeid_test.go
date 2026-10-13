package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/run"
)

const (
	openerChangeID = "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	laterChangeID  = "yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	otherChangeID  = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	resumeChangeID = "wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww"
	prURL7         = "https://github.com/acme/widget/pull/7"
)

// seedSession saves states into a fresh session store under the test HOME.
func seedSession(t *testing.T, sessionID string, states ...*run.State) *run.HomeDirStore {
	t.Helper()
	store, err := run.NewHomeDirStore(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if err := store.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestChooseChangeID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	str := func(s string) *string { return &s }

	// The PR was opened in another coordinator session. A pre-change-ID run
	// on the same PR and a same-named branch in another repo are older still.
	seedSession(t, "20261001-0900-opener",
		&run.State{ID: "20261001-0900-oldstate", Branch: "fix-thing-1234", PRURL: str(prURL7), CreatedAt: "2026-10-01T08:00:00Z"},
		&run.State{ID: "20261001-0900-otherrepo", Branch: "fix-thing-1234", PRURL: str("https://github.com/acme/other/pull/7"), ChangeID: otherChangeID, CreatedAt: "2026-10-01T08:30:00Z"},
		&run.State{ID: "20261001-0900-opener", Branch: "fix-thing-1234", PRURL: str(prURL7), ChangeID: openerChangeID, CreatedAt: "2026-10-01T09:00:00Z"},
		&run.State{ID: "20261002-0900-fix", Type: "pr-fix", Branch: "fix-thing-1234", PRURL: str(prURL7), ChangeID: laterChangeID, CreatedAt: "2026-10-02T09:00:00Z"},
	)
	store := seedSession(t, "20261013-1500-current",
		&run.State{ID: "20261013-1400-resumed", Branch: "resumed-1111", ChangeID: resumeChangeID, CreatedAt: "2026-10-13T14:00:00Z"},
		&run.State{ID: "20261013-1410-legacy", Branch: "legacy-2222", CreatedAt: "2026-10-13T14:10:00Z"},
	)

	t.Run("--pr inherits from the run that opened the PR", func(t *testing.T) {
		c, err := chooseChangeID(ctx, store, changeIDInput{IsPRFix: true, PRURL: prURL7, PRBranch: "fix-thing-1234"})
		if err != nil {
			t.Fatal(err)
		}
		if c.ID != openerChangeID || !strings.Contains(c.From, "20261001-0900-opener") {
			t.Errorf("got %+v, want the opener's %s", c, openerChangeID)
		}
	})

	t.Run("without PR URLs, --pr matches the branch in the same repo only", func(t *testing.T) {
		seedSession(t, "20261003-0900-nourl",
			&run.State{ID: "20261003-0800-elsewhere", Branch: "shared-name", TargetRepo: str("acme/other"), ChangeID: otherChangeID, CreatedAt: "2026-10-03T08:00:00Z"},
			&run.State{ID: "20261003-0830-unknown", Branch: "shared-name", ChangeID: resumeChangeID, CreatedAt: "2026-10-03T08:30:00Z"},
			&run.State{ID: "20261003-0900-here", Branch: "shared-name", TargetRepo: str("acme/widget"), ChangeID: laterChangeID, CreatedAt: "2026-10-03T09:00:00Z"},
		)
		c, err := chooseChangeID(ctx, store, changeIDInput{IsPRFix: true, PRBranch: "shared-name", TargetRepo: str("acme/widget")})
		if err != nil {
			t.Fatal(err)
		}
		if c.ID != laterChangeID {
			t.Errorf("got %+v, want acme/widget's %s", c, laterChangeID)
		}
	})

	t.Run("--resume-from inherits from the resumed run", func(t *testing.T) {
		c, err := chooseChangeID(ctx, store, changeIDInput{ResumeFrom: "20261013-1400-resumed"})
		if err != nil {
			t.Fatal(err)
		}
		if c.ID != resumeChangeID {
			t.Errorf("got %+v, want %s", c, resumeChangeID)
		}
	})

	t.Run("continuing a run that predates change IDs generates one and says so", func(t *testing.T) {
		c, err := chooseChangeID(ctx, store, changeIDInput{ResumeFrom: "20261013-1410-legacy"})
		if err != nil {
			t.Fatal(err)
		}
		if !run.ValidChangeID(c.ID) || !strings.Contains(c.From, "no change ID is recorded for run 20261013-1410-legacy") {
			t.Errorf("got %+v, want a new ID noting the legacy run", c)
		}
	})

	t.Run("fresh launch generates one", func(t *testing.T) {
		c, err := chooseChangeID(ctx, store, changeIDInput{})
		if err != nil {
			t.Fatal(err)
		}
		if !run.ValidChangeID(c.ID) || c.From != "new" {
			t.Errorf("got %+v, want a new ID", c)
		}
		for _, known := range []string{openerChangeID, laterChangeID, otherChangeID, resumeChangeID} {
			if c.ID == known {
				t.Errorf("fresh launch reused %s", known)
			}
		}
	})

	t.Run("--pr with no recorded run reads the PR head commit", func(t *testing.T) {
		_, _, worktree, branch := setupBareRemote(t)
		runGitCmd(t, worktree, "add", "-A")
		runGitCmd(t, worktree, "commit", "-m", "wip\n\nChange-Id: "+laterChangeID+"\nKlaus-Run: 20261012-0000-elsewhere")

		c, err := chooseChangeID(ctx, store, changeIDInput{IsPRFix: true, PRURL: "https://github.com/acme/widget/pull/99", PRBranch: branch, Worktree: worktree, DefaultBranch: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if c.ID != laterChangeID {
			t.Errorf("got %+v, want the head commit's %s", c, laterChangeID)
		}
	})

	t.Run("--pr whose head is on the default branch generates one", func(t *testing.T) {
		_, repo, worktree, branch := setupBareRemote(t)
		// main's tip belongs to a change that already landed.
		runGitCmd(t, repo, "commit", "--allow-empty", "-m", "landed\n\nChange-Id: "+otherChangeID)
		runGitCmd(t, repo, "push", "origin", "main")
		runGitCmd(t, worktree, "reset", "--hard", "origin/main")

		c, err := chooseChangeID(ctx, store, changeIDInput{IsPRFix: true, PRURL: "https://github.com/acme/widget/pull/98", PRBranch: branch, Worktree: worktree, DefaultBranch: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if c.ID == otherChangeID || !strings.Contains(c.From, "no change ID is recorded for the PR") {
			t.Errorf("got %+v, want a new ID, not the landed change's", c)
		}
	})
}

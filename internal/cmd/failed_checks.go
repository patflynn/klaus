package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/patflynn/klaus/internal/checksfix"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/restack"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
	"github.com/spf13/cobra"
)

var failedChecksCmd = &cobra.Command{
	Use:   "failed-checks",
	Short: "Dispatch runs to fix the valley checks that failed on ready changes klaus owns",
	Long: `Reads the checks of the ready changes in the valley repos the session's
runs deliver to, and launches a run to fix each failed head of a change
klaus owns, as the pipeline dispatches a fix agent for a PR's failing CI.
The dashboard does this after each integration check, about once a minute;
run it by hand where no dashboard runs.

The valley lists, on a ready change's active version, where the host's
check worker stands on its required checks: pending, failed or passed. A
valley from before checks were listed lists none, and klaus does nothing.
A change is klaus's when its record's owner is klaus's principal, the
repo's git config valley.principal.

For each failed head, klaus:

  - does nothing while a run, in any session, is working on the change or
    its branch;
  - acts once on each failed head, whatever came of it;
  - counts a fix run whose change's next version still fails as a failed
    attempt, and stops after 3 attempts, until the change's checks pass;
  - records the dispatch, and the ID of the run it is about to launch, in
    the session's checks-dispatches.json, and launches nothing if it can't;
  - launches 'klaus launch [--repo <project>] --change <change-id>
    --run-id <id> --expect-owner <principal> --expect-branch <branch>
    --expect-head <head> --reason checks-failed <brief>' from a clone of the
    repo. Launch returns the change to draft before the run starts, and
    refuses a change that moved since klaus read it.

The brief names the failed checks and what the worker said of each, how to
read the worker's build output ('valley telemetry --json read --source
worker --logs'), that the run amends the change's single commit, and that
it builds every check 'valley checks --names origin/main HEAD' lists before
it pushes.

The dashboard reports change:checks-failed and change:checks-passed for
each verdict; klaus status and the dashboard show the checks in the CI
column. Prints one line per change klaus owns whose checks failed.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := sessionStore()
		if err != nil {
			return err
		}
		hds, ok := store.(*run.HomeDirStore)
		if !ok {
			return fmt.Errorf("failed-checks needs a session directory")
		}
		lines, err := checksPass(cmd.Context(), hds, nil)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			fmt.Println("No failed checks on ready changes klaus owns.")
		}
		for _, l := range lines {
			fmt.Println(l)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(failedChecksCmd)
}

// checksPass runs a checks pass over the valley repos of the session's
// runs, reading their checks in listed, or with nil, listing each repo,
// and dispatching a fix run for each failed head of a change klaus owns.
func checksPass(ctx context.Context, store *run.HomeDirStore, listed *valley.Changes) ([]string, error) {
	states, err := store.List()
	if err != nil {
		return nil, err
	}
	root, _ := git.RepoRoot()
	repos := restack.Repos(states, root)
	if len(repos) == 0 {
		return nil, nil
	}
	unlock, err := checksfix.Lock(store.BaseDir())
	if err != nil {
		return nil, err
	}
	defer unlock()
	// The ledger holds the attempts: a pass that can't read it dispatches
	// nothing, rather than start them over.
	ledger, err := checksfix.LoadLedger(store.BaseDir())
	if err != nil {
		return nil, fmt.Errorf("reading the checks ledger: %w", err)
	}
	all, err := run.ListAllSessions()
	if err != nil {
		all = states
	}
	d := checksfix.Deps{
		List:      valley.ListRepoChecks,
		Principal: valley.Principal,
		NewRunID:  run.GenID,
		Launch:    launchChecksFix,
		Running:   restack.RunningOn(all, run.DefaultTmuxDeps()),
		Now:       time.Now,
	}
	return checksfix.Pass(ctx, d, ledger, repos, listed, true), nil
}

// launchChecksFix launches a fix run with this klaus binary, under the run
// ID klaus recorded, expecting the change at the owner, branch and head
// klaus read.
func launchChecksFix(ctx context.Context, l checksfix.Launch) error {
	self, err := os.Executable()
	if err != nil {
		self = "klaus"
	}
	args := []string{"launch"}
	if l.Project != "" {
		args = append(args, "--repo", l.Project)
	}
	args = append(args, "--change", l.ChangeID, "--run-id", l.RunID,
		"--expect-owner", l.Owner, "--expect-branch", l.Branch, "--expect-head", l.Head,
		"--reason", event.DispatchChecksFailed, "--", l.Prompt)
	return runLaunch(ctx, self, l.Dir, args)
}

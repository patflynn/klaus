package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/restack"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
	"github.com/spf13/cobra"
)

var restackConflictsCmd = &cobra.Command{
	Use:   "restack-conflicts",
	Short: "Dispatch runs to resolve the restack conflicts a valley reports on changes klaus owns",
	Long: `Reads the restack reports of the valley repos the session's runs deliver
to, and launches a run to resolve each current conflict on a change klaus
owns. The dashboard does this after each integration check, about once a
minute; run it by hand where no dashboard runs.

A valley's restacker rebases ready changes onto main after each landing, and
when a rebase conflicts it reports the conflict beside the change. A change
is klaus's when its record's owner is klaus's principal, the repo's git
config valley.principal; who uploaded its latest version doesn't count,
since the restacker uploads the versions it rebases. A report counts only
while the valley says it is current, and while what the listing shows of
the change (its record, active version and head) matches the report.

For each such conflict, unless klaus leaves it alone (below), klaus:

  1. reads the change and origin's refs again: main, the report, and each
     rebased change's record and branch, and skips the report if any moved;
  2. returns the change to draft ('valley unready'), and launches nothing
     unless the valley then lists it as a draft at the same head;
  3. launches 'klaus launch [--repo <project>] --change <change-id>
     --reason restack-conflict <brief>' from a clone of the repo, which
     keeps launch's own refusals: a run already working on the change or
     its branch, or a branch with work origin lacks.

The brief names the change, its version, head and branch, and main's tip:
what klaus read itself. Nothing a report says is run, and none of its text
is passed on; the run finds the conflicts by rebasing, and pushes the
rebased head with the leased push its instructions give.

As the pipeline does for a PR's conflicts, klaus never starts a second run
on a change, waits 60 seconds after each dispatch, and stops after 3 runs,
until a report says the change restacked cleanly. It acts once on one set
of inputs, the change's head and main's tip: an unchanged conflict is not
retried, nor is one whose unready or launch failed. klaus leaves a conflict
alone when it is in another change below this one (a change whose owner
isn't klaus is never taken over), or when the change sits on other changes.

Each dispatch emits change:restack-dispatched under the new run's ID; a
conflict klaus stops short of resolving (attempts spent, unready or launch
failed) emits change:restack-stalled once. What klaus did is kept in the
session's restack-dispatches.json; klaus status and the dashboard show it.

Prints one line per conflict on a change klaus owns.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := sessionStore()
		if err != nil {
			return err
		}
		hds, ok := store.(*run.HomeDirStore)
		if !ok {
			return fmt.Errorf("restack-conflicts needs a session directory")
		}
		views, err := restackPass(cmd.Context(), hds, nil, true)
		if err != nil {
			return err
		}
		if len(views) == 0 {
			fmt.Println("No restack conflicts on changes klaus owns.")
		}
		for _, v := range views {
			fmt.Println(v)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(restackConflictsCmd)
}

// restackLaunchTimeout bounds one resolution run's launch.
const restackLaunchTimeout = 5 * time.Minute

// restackPass runs a restack pass over the valley repos of the session's
// runs, reading their reports in listed, or with nil, listing each repo.
// With act it holds the ledger's lock, dispatches and records what it did;
// without, it only reports.
func restackPass(ctx context.Context, store *run.HomeDirStore, listed *valley.Changes, act bool) ([]restack.View, error) {
	states, err := store.List()
	if err != nil {
		return nil, err
	}
	root, _ := git.RepoRoot()
	repos := restack.Repos(states, root)
	if len(repos) == 0 {
		return nil, nil
	}
	if act {
		unlock, err := restack.Lock(store.BaseDir())
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	// The ledger holds the attempts and cooldowns: a pass that can't read
	// it dispatches nothing, rather than start them over.
	ledger, err := restack.LoadLedger(store.BaseDir())
	if err != nil && act {
		return nil, fmt.Errorf("reading the restack ledger: %w", err)
	}
	if err != nil {
		slog.Warn("reading the restack ledger", "err", err)
	}
	all, err := run.ListAllSessions()
	if err != nil {
		all = states
	}
	events := event.NewLog(store.BaseDir())
	emit := func(runID, typ string, data map[string]interface{}) {
		if err := events.Emit(event.New(runID, typ, data)); err != nil {
			slog.Warn("emitting a restack event", "type", typ, "err", err)
		}
	}
	d := restack.Default(launchResolution, restack.RunningOn(all, run.DefaultTmuxDeps()), emit)
	return restack.Pass(ctx, d, ledger, repos, listed, act), nil
}

// launchResolution launches a resolution run with this klaus binary, and
// returns its run ID ("unknown" when launch didn't print it).
func launchResolution(ctx context.Context, l restack.Launch) (string, error) {
	self, err := os.Executable()
	if err != nil {
		self = "klaus"
	}
	args := []string{"launch"}
	if l.Project != "" {
		args = append(args, "--repo", l.Project)
	}
	args = append(args, "--change", l.ChangeID, "--reason", event.DispatchRestackConflict, "--", l.Prompt)
	ctx, cancel := context.WithTimeout(ctx, restackLaunchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Dir = l.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("klaus launch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "Launching agent "); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				return strings.TrimSuffix(f[0], "..."), nil
			}
		}
	}
	return "unknown", nil
}

// restackViews indexes views by change, as the dashboard and status look
// them up.
func restackViews(views []restack.View) map[integration.ChangeKey]restack.View {
	out := make(map[integration.ChangeKey]restack.View, len(views))
	for _, v := range views {
		out[integration.ChangeKey{Repo: v.Repo, ChangeID: v.ChangeID}] = v
	}
	return out
}

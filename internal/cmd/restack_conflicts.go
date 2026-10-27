package cmd

import (
	"context"
	"errors"
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
	"github.com/patflynn/klaus/internal/tmux"
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
the change (its record, active version, head and branch) matches the
report, and each other change the report rebased is listed on the branch
the report names for it.

For each such conflict, unless klaus leaves it alone (below), klaus:

  1. reads the change and origin's refs again: main, the report, and each
     rebased change's record and the branch its record names, and skips
     the report if any moved;
  2. records the attempt, and the ID of the run it is about to launch, in
     its ledger, and does nothing if it can't;
  3. returns the change to draft ('valley unready'), and launches nothing
     unless the valley then lists it as klaus's draft at the same head and
     branch;
  4. launches 'klaus launch [--repo <project>] --change <change-id>
     --run-id <id> --expect-owner <principal> --expect-branch <branch>
     --expect-head <head> --expect-draft --reason restack-conflict <brief>'
     from a clone of the repo. Launch checks all of it again, fetching the
     branch and asking the valley under its continuation lock, before it
     creates the worktree and again as the worker starts, and fails rather
     than start a worker on a change that was readied or moved meanwhile,
     or that it couldn't check. It keeps its own refusals too: a run
     already working on the change or its branch, or a branch with work
     origin lacks.

The brief names the change, its version, head and branch, and main's tip:
what klaus read itself. Nothing a report says is run, and none of its text
is passed on; the run finds the conflicts by rebasing, and pushes the
rebased head with the leased push its instructions give.

As the pipeline does for a PR's conflicts, klaus never starts a second run
on a change, waits 60 seconds after each dispatch, and stops after 3 runs,
until a report says the change restacked cleanly: one reset per clean
report, and only while origin still holds that report. It acts once on
each set of inputs, the change's head and main's tip, whatever came
between: an unchanged conflict is not retried, nor one that comes back to
inputs klaus acted on before, nor one whose unready or launch failed; it
shows what came of those inputs then. Once
no run is working on a change whose attempts are spent, klaus says it
stopped, even if the last run left the conflict as it was.

An attempt is given back only when klaus knows no worker started: it
stopped before launching, or launch exited with status 3. Any other launch
failure keeps the attempt and the cooldown, and so does a pass that ends
mid-launch: either leaves the launch pending in the ledger, and the change
busy, until a run by its ID is recorded (a dispatch) or klaus proves no
worker of it runs: no launch holds the clone's continuation lock, no run
by the ID is recorded, and no live tmux pane's command names it. Time alone
settles nothing.

klaus leaves a conflict alone when it is in another change below this one
(a change whose owner isn't klaus is never taken over), or when the change
sits on other changes.

Each dispatch emits change:restack-dispatched under the new run's ID; a
conflict klaus stops short of resolving (attempts spent, unready or launch
failed) emits change:restack-stalled once. What klaus did is kept in the
session's restack-dispatches.json; klaus status and the dashboard show it.
The ledger drops a change's entry once the change lands or is withdrawn,
unless a launch for it is pending, and keeps what came of a change's latest
16 sets of inputs; older ones may be acted on again.

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
	d := restack.Default(launchResolution, runRecorded, restack.RunningOn(all, run.DefaultTmuxDeps()), workerGone, emit)
	return restack.Pass(ctx, d, ledger, repos, listed, act), nil
}

// launchResolution launches a resolution run with this klaus binary,
// under the run ID klaus recorded, in launch's strict mode: launch checks
// the change is still the owner's draft at the branch and head klaus
// checked before it starts the worker. Launch's exit status 3 says it
// failed before starting one (restack.ErrNotStarted).
func launchResolution(ctx context.Context, l restack.Launch) error {
	self, err := os.Executable()
	if err != nil {
		self = "klaus"
	}
	args := []string{"launch"}
	if l.Project != "" {
		args = append(args, "--repo", l.Project)
	}
	args = append(args, "--change", l.ChangeID, "--run-id", l.RunID,
		"--expect-owner", l.Owner, "--expect-branch", l.Branch, "--expect-head", l.Head, "--expect-draft",
		"--reason", event.DispatchRestackConflict, "--", l.Prompt)
	ctx, cancel := context.WithTimeout(ctx, restackLaunchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Dir = l.Dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	why := launchError(out)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == exitNoWorker {
		return fmt.Errorf("klaus launch: %w: %s", restack.ErrNotStarted, why)
	}
	return fmt.Errorf("klaus launch: %w: %s", err, why)
}

// launchError is what a failed launch's output says went wrong: its
// "Error: " line, else all of it.
func launchError(out []byte) string {
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if msg, ok := strings.CutPrefix(lines[i], "Error: "); ok {
			return msg
		}
	}
	return strings.TrimSpace(string(out))
}

// runRecorded reports whether a run by runID is recorded in any session.
func runRecorded(runID string) bool {
	states, err := run.ListAllSessions()
	return err == nil && findRun(states, runID) != nil
}

// workerGone reports whether klaus can prove that no worker of run runID
// runs, nor is about to start, from the clone at dir: no launch holds the
// clone's continuation lock, under which launch checks for live holders,
// starts its worker and records its run (and stops a worker whose run it
// couldn't record); no run by the ID is recorded, in any session; and no
// live tmux pane runs a command that names the run, as every worker's
// does. A launch still on its way to the lock is no worker yet: under the
// lock it finds any run recorded meanwhile, and starts none beside it. Any
// read that fails is doubt: false.
func workerGone(ctx context.Context, dir, runID string) bool {
	release, ok := tryLockContinuation(ctx, dir)
	if !ok {
		return false
	}
	defer release()
	states, err := run.ListAllSessions()
	if err != nil || findRun(states, runID) != nil {
		return false
	}
	live, err := tmux.PaneStartedWith(ctx, runID)
	return err == nil && !live
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

package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/patflynn/klaus/internal/consult"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup <run-id> | --done | --all",
	Short: "Remove worktrees, panes, and state",
	Long: `Cleans up a run by killing its tmux pane, removing its worktree,
deleting local branches, and removing state files.

Use --done to clean up what's finished: the runs whose PR or change is
merged, closed, integrated, withdrawn or gone, as klaus status shows them,
and the older runs of a PR or change still open. It keeps the latest run of
each open PR or change, a run that needs attention, and a running run, and
prints a line for each run it removes or keeps, with why.

Use --all to clean up all runs, and the session's consult thread snapshots
(see klaus consult --repo) not in use; a thread makes a new one on its next
turn. Runs with active tmux panes are skipped by default; pass --force to
remove them anyway. With --all, the detached
agents tmux session is removed too — but only once no remaining run holds a
pane in it and no pane in it is still running a command, so a session with
live agents is never torn down.

Each run cleanup removes leaves its cost and duration in the session's
ledger, so klaus status and the dashboard still count them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		done, _ := cmd.Flags().GetBool("done")
		force, _ := cmd.Flags().GetBool("force")
		ctx := cmd.Context()
		tmuxClient := tmux.NewExecClient()
		gitClient := git.NewExecClient()

		root, _ := git.RepoRoot() // may be empty outside a repo

		deps := DefaultCleanupDeps(tmuxClient)

		store, err := sessionStore()
		if err != nil {
			return err
		}

		if done {
			if all || len(args) > 0 {
				return fmt.Errorf("--done takes no run ID and not --all")
			}
			return cleanupDone(ctx, root, store, gitClient, force, deps, tmuxClient)
		}
		if all {
			if hs, ok := store.(*run.HomeDirStore); ok {
				removeConsultSnapshots(ctx, consult.NewStore(hs.BaseDir()), cmd.OutOrStdout())
			}
			return cleanupAll(ctx, root, store, gitClient, force, deps, tmuxClient)
		}

		if len(args) != 1 {
			return fmt.Errorf("usage: klaus cleanup <run-id>, klaus cleanup --done or klaus cleanup --all")
		}

		return cleanupOne(ctx, root, store, gitClient, args[0], force, deps, tmuxClient)
	},
}

func cleanupAll(ctx context.Context, root string, store run.StateStore, gitClient git.Client, force bool, deps CleanupDeps, tc tmux.Client) error {
	states, err := store.List()
	if err != nil {
		return err
	}
	if len(states) == 0 {
		fmt.Println("No runs to clean up.")
		return nil
	}
	for _, s := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := cleanupOne(ctx, root, store, gitClient, s.ID, force, deps, tc); err != nil {
			fmt.Printf("  warning: failed to clean up %s: %v\n", s.ID, err)
		}
	}
	killEmptyAgentsSession(ctx, store, tc)
	return nil
}

// CleanupDeps holds dependencies for cleanup operations.
type CleanupDeps struct {
	IsRunActive func(state *run.State) bool
}

// DefaultCleanupDeps returns CleanupDeps wired to real implementations.
func DefaultCleanupDeps(tc tmux.Client) CleanupDeps {
	return CleanupDeps{
		IsRunActive: func(state *run.State) bool {
			return defaultIsRunActive(state, tc)
		},
	}
}

// defaultIsRunActive reports whether the run has a live, non-idle tmux pane or is the current session.
func defaultIsRunActive(state *run.State, tc tmux.Client) bool {
	ctx := context.Background()
	if state.Type == "session" {
		if sid := os.Getenv(sessionIDEnv); sid != "" && state.ID == sid {
			return true
		}
	}

	if state.IsAgentRunningWith(run.TmuxDepsFor(ctx, tc)) {
		return true
	}

	if state.DashboardPane != nil && tc.PaneExists(ctx, *state.DashboardPane) {
		return true
	}
	return false
}

func cleanupOne(ctx context.Context, root string, store run.StateStore, gitClient git.Client, id string, force bool, deps CleanupDeps, tc tmux.Client) error {
	return cleanupOneTo(os.Stdout, ctx, root, store, gitClient, id, force, deps, tc)
}

// cleanupOneTo is cleanupOne, reporting to w.
func cleanupOneTo(w io.Writer, ctx context.Context, root string, store run.StateStore, gitClient git.Client, id string, force bool, deps CleanupDeps, tc tmux.Client) error {
	state, err := store.Load(id)
	if err != nil {
		return fmt.Errorf("no run found with id: %s", id)
	}

	if !force && deps.IsRunActive(state) {
		fmt.Fprintf(w, "skipping %s (still running) — use --force to remove\n", id)
		return nil
	}

	fmt.Fprintf(w, "Cleaning up %s...\n", id)

	// Kill tmux pane if alive
	if state.TmuxPane != nil && tc.PaneExists(ctx, *state.TmuxPane) {
		if err := tc.KillPane(ctx, *state.TmuxPane); err == nil {
			fmt.Fprintln(w, "  killed tmux pane")
		} else {
			slog.Warn("failed to kill tmux pane", "id", id, "pane", *state.TmuxPane, "err", err)
		}
	}

	// Kill dashboard pane if alive
	if state.DashboardPane != nil && tc.PaneExists(ctx, *state.DashboardPane) {
		if err := tc.KillPane(ctx, *state.DashboardPane); err == nil {
			fmt.Fprintln(w, "  killed dashboard pane")
		} else {
			slog.Warn("failed to kill dashboard pane", "id", id, "pane", *state.DashboardPane, "err", err)
		}
	}

	// For cross-repo runs, git ops target the clone dir instead of the host root
	gitRoot := root
	if state.CloneDir != nil {
		gitRoot = *state.CloneDir
	}

	// Remove worktree
	if state.Worktree != "" {
		if err := gitClient.WorktreeRemove(ctx, gitRoot, state.Worktree); err == nil {
			fmt.Fprintln(w, "  removed worktree")
		} else {
			slog.Warn("failed to remove worktree", "id", id, "worktree", state.Worktree, "err", err)
		}
	}
	removeScratchDir(store, id)

	// Delete local branch
	if state.Branch != "" {
		if err := gitClient.BranchDelete(ctx, gitRoot, state.Branch); err == nil {
			fmt.Fprintln(w, "  deleted local branch")
		} else {
			slog.Warn("failed to delete local branch", "id", id, "branch", state.Branch, "err", err)
		}
	}

	// Keep its spend in the session's totals, then remove its state file.
	if hs, ok := store.(*run.HomeDirStore); ok && state.Type != "session" {
		if err := run.RecordCleared(hs.BaseDir(), state); err != nil {
			return fmt.Errorf("recording %s's spend: %w", id, err)
		}
	}
	if err := store.Delete(id); err == nil {
		fmt.Fprintln(w, "  removed state file")
	} else {
		slog.Warn("failed to remove state file", "id", id, "err", err)
	}

	fmt.Fprintln(w, "  done.")
	return nil
}

func init() {
	cleanupCmd.Flags().Bool("all", false, "Clean up all runs")
	cleanupCmd.Flags().Bool("done", false, "Clean up the runs of finished PRs and changes, and older runs of open ones")
	cleanupCmd.Flags().Bool("force", false, "Remove runs even if they are still running")
	rootCmd.AddCommand(cleanupCmd)
}

// doneGroup is the runs of one PR or change, as klaus status or the
// dashboard groups them.
type doneGroup struct {
	label string // the PR's number, the change's handle or its change ID's prefix
	// finished is merged, closed, integrated, withdrawn or gone once the PR
	// or change is finished; "" while it is open.
	finished string
	runs     []*run.State
}

// doneVerdict is what cleanup --done does with one run, and why.
type doneVerdict struct {
	state  *run.State
	remove bool
	why    string
}

// planDone decides, for each run of groups and each run of none (loose),
// whether cleanup --done removes it: every run of a finished PR or change,
// and every older run of an open one; a group may be one finished run with
// neither, as a pushed branch that landed. It keeps the latest run of an open PR
// or change, a run that is running or needs attention, and a loose run.
func planDone(groups []doneGroup, loose []*run.State, running, attention func(*run.State) bool) []doneVerdict {
	var out []doneVerdict
	for _, g := range groups {
		var latest *run.State
		for _, s := range g.runs {
			if latest == nil || createdBefore(latest, s) {
				latest = s
			}
		}
		for _, s := range g.runs {
			v := doneVerdict{state: s}
			switch {
			case running(s):
				v.why = "running, on " + g.label
			case attention(s):
				v.why = "needs attention, on " + g.label
			case g.finished != "":
				v.remove, v.why = true, g.label+" "+g.finished
			case s == latest:
				v.why = "latest run of " + g.label + ", still open"
			default:
				v.remove, v.why = true, "older run of "+g.label
			}
			out = append(out, v)
		}
	}
	for _, s := range loose {
		why := "no PR or change"
		if running(s) {
			why = "running"
		}
		out = append(out, doneVerdict{state: s, why: why})
	}
	return out
}

// finishedStatus is a finished PR's or change's word for a run status, ""
// when the status leaves the PR or change open.
func finishedStatus(status string) string {
	switch status {
	case "merged", "closed", "integrated", "withdrawn":
		return status
	case "branch-gone":
		return "gone"
	}
	return ""
}

// finishedVerdict is a finished change's word for its active version's
// verdict, "" while the change is open.
func finishedVerdict(v integration.Verdict) string {
	switch {
	case v.Landed():
		return "integrated"
	case v == integration.Withdrawn:
		return "withdrawn"
	case v == integration.Gone:
		return "gone"
	}
	return ""
}

// cleanupDone removes the runs planDone picks, judged as klaus status
// judges them, and says what it removed and kept.
func cleanupDone(ctx context.Context, root string, store run.StateStore, gitClient git.Client, force bool, deps CleanupDeps, tc tmux.Client) error {
	states, err := store.List()
	if err != nil {
		return err
	}
	var listed []*run.State
	for _, s := range states {
		if s.Type != "session" {
			listed = append(listed, s)
		}
	}
	if len(listed) == 0 {
		fmt.Println("No runs to clean up.")
		return nil
	}
	statuses, report, err := runStanding(ctx, store, listed)
	if err != nil {
		return err
	}
	status := make(map[*run.State]string, len(listed))
	for i, s := range listed {
		status[s] = statuses[i]
	}
	changeOf := changeKeys(listed)
	prs, handles := changePRs(listed), changeHandles(listed, report.Changes)
	byKey := make(map[string]*doneGroup)
	var groups []*doneGroup
	var loose []*run.State
	for _, s := range listed {
		key := ""
		if k, ok := changeOf(s); ok {
			key = k.Repo + "@" + k.ChangeID
		} else if s.PRURL != nil {
			key = *s.PRURL
		} else if finishedStatus(status[s]) != "" {
			key = s.ID // a branch pushed without a PR or change, landed or gone
		}
		if key == "" {
			loose = append(loose, s)
			continue
		}
		g := byKey[key]
		if g == nil {
			g = &doneGroup{label: changeLabel(s, changeOf, prs, handles)}
			if g.label == "-" {
				g.label = "its branch"
			}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.runs = append(g.runs, s)
	}
	for _, g := range groups {
		// A change the valley lists is finished as its active version
		// stands; otherwise as its newest run with a PR or a push does.
		var newest *run.State
		for _, s := range g.runs {
			if view := report.Changes[valleyKey(s)]; view.Change != nil {
				g.finished, newest = finishedVerdict(view.Verdict), nil
				break
			}
			if (s.PRURL != nil || s.PushedSHA != nil || s.Type == "track") && (newest == nil || createdBefore(newest, s)) {
				newest = s
			}
		}
		if newest != nil {
			g.finished = finishedStatus(status[newest])
		}
	}
	plain := make([]doneGroup, len(groups))
	for i, g := range groups {
		plain[i] = *g
	}
	plan := planDone(plain, loose,
		func(s *run.State) bool { return status[s] == "running" || deps.IsRunActive(s) },
		func(s *run.State) bool { return status[s] == "needs-attention" })
	for _, v := range plan {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !v.remove {
			fmt.Printf("kept    %s  %s\n", v.state.ID, v.why)
			continue
		}
		fmt.Printf("removed %s  %s\n", v.state.ID, v.why)
		if err := cleanupOneTo(io.Discard, ctx, root, store, gitClient, v.state.ID, force, deps, tc); err != nil {
			fmt.Printf("  warning: failed to clean up %s: %v\n", v.state.ID, err)
		}
	}
	return nil
}

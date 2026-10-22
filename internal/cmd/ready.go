package cmd

import (
	"errors"
	"fmt"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/patflynn/klaus/internal/valley"
	"github.com/spf13/cobra"
)

var readyCmd = &cobra.Command{
	Use:   "ready <run-id|handle>",
	Short: "Mark a valley change ready for approval once its review passes",
	Long: `Marks a valley change ready for approval: typically the draft that
finalize leaves awaiting review when "ready_at_finalize" is false in
~/.klaus/config.json or the project's .klaus/config.json. Run it once the
change's review passes; until then approve refuses the change, and klaus
status and the dashboard show it awaiting review.

<run-id|handle> is a run ID, which names that run's change, or the change's
valley handle, its change ID or a prefix of it, as the runs of this session
record them. A ref that names several changes is an error listing them.

It marks the change's current head ready: its active version's when the
valley lists the change, else the head the newest run on it, in any session,
pushed. That is 'valley ready --change <change-id> --head <head> <branch>',
run in the run's clone, within 30 seconds. A draft at its active version's
head is ready again at that version. klaus ready then records the version on
the run of this session that pushed the head, if any, and emits
agent:change-ready, with "via": "klaus ready", as finalize does when it marks
a change ready.

It refuses while an agent in any session is running on the change, as a
follow-up replacing its head is, and when the run's repo doesn't deliver to a
valley. It takes the lock that launches continuing a change in the same clone
take, so a follow-up launch can't start between that check and the ready.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		store, err := sessionStore()
		if err != nil {
			return err
		}
		states, err := store.List()
		if err != nil {
			return err
		}
		s, err := readyTarget(states, args[0])
		if err != nil {
			return err
		}
		if !run.ValidChangeID(s.ChangeID) {
			return fmt.Errorf("run %s has no change ID (its state predates change IDs)", s.ID)
		}
		repo, dir := integration.RepoName(s), branchDir(s)
		if repo == "" {
			return fmt.Errorf("can't tell which repo run %s is in", s.ID)
		}
		// A follow-up launch in this clone takes the same lock from its
		// check for a running holder through its run's registration, so it
		// either returns the change to draft after this, or is seen here.
		// The change's head is read under it, from the runs as they stand
		// now: a follow-up may have pushed a newer head since.
		unlock, err := lockContinuation(ctx, dir)
		if err != nil {
			return err
		}
		defer unlock()
		if states, err = store.List(); err != nil {
			return err
		}
		// Every session's runs, this one's first, as launch reads them.
		runs := append([]*run.State(nil), states...)
		mine := make(map[string]bool)
		for _, c := range states {
			mine[c.ID] = true
		}
		all, _ := run.ListAllSessions()
		for _, c := range all {
			if !mine[c.ID] {
				runs = append(runs, c)
			}
		}
		t, err := resolveChange(s.ID, changeSources{
			Runs:   runs,
			Repo:   repo,
			Valley: func() (*valley.Changes, error) { return valley.ListRepo(ctx, dir) },
		})
		if err != nil {
			return err
		}
		if t.Branch == "" || t.Head == "" {
			return fmt.Errorf("change %s has no known head: no run on it recorded a pushed branch, and no valley lists one", t.Name)
		}
		deps := run.TmuxDepsFor(ctx, tmux.NewExecClient())
		running := func(r *run.State) bool { return r.IsAgentRunningWith(deps) }
		if h := liveHolder(runs, running, "", t.ID, repo); h != nil {
			return fmt.Errorf("run %s is still running on change %s; ready the change once it has pushed and its review has passed", h.ID, t.Name)
		}
		if !valley.Detect(ctx, dir, t.Branch, t.Head) {
			return fmt.Errorf("change %s isn't on a valley klaus can mark changes ready on: %s's origin isn't one, its head carries no valley policy, or valley isn't on PATH", t.Name, dir)
		}
		r, err := valley.Ready(ctx, dir, t.ID, t.Head, t.Branch)
		if err != nil {
			return err
		}

		// The run that pushed the head holds the version, when it is this
		// session's; one marked ready elsewhere, or pushed by a run of
		// another session, is recorded on none here.
		var pusher *run.State
		for _, c := range t.Runs {
			if mine[c.ID] && c.PushedSHA != nil && *c.PushedSHA == t.Head {
				pusher = c
				break
			}
		}
		title := ""
		if pusher != nil {
			rec := pusher.Valley
			if rec == nil {
				rec = &run.Valley{Title: commitSubject(ctx, dir, t.Head)}
				pusher.Valley = rec
			}
			rec.Repo, rec.Handle, rec.Version, rec.Draft, rec.Error = r.Repo, r.Handle, r.Version, false, ""
			title = rec.Title
			if err := store.Save(pusher); err != nil {
				return fmt.Errorf("%s %s v%d is ready, but recording it on run %s failed: %w", r.Repo, r.Handle, r.Version, pusher.ID, err)
			}
		} else {
			title = commitSubject(ctx, dir, t.Head)
		}
		on := s
		if pusher != nil {
			on = pusher
		}
		if hds, ok := store.(*run.HomeDirStore); ok {
			emitRunEvent(hds.BaseDir(), on, event.AgentChangeReady, map[string]interface{}{
				"id":        on.ID,
				"repo":      r.Repo,
				"change_id": t.ID,
				"handle":    r.Handle,
				"version":   r.Version,
				"title":     title,
				"head":      t.Head,
				"via":       "klaus ready",
			})
		}
		fmt.Printf("%s %s v%d ready (run %s)\n", r.Repo, r.Handle, r.Version, on.ID)
		return nil
	},
}

// readyTarget finds the run whose change ref names, among a session's runs:
// the run whose ID ref is, or else the newest run on the change whose
// recorded valley handle ref is, or whose change ID starts with ref.
func readyTarget(states []*run.State, ref string) (*run.State, error) {
	if ref == "" {
		return nil, errors.New("name a run ID, a valley handle or a change ID")
	}
	if s := findRun(states, ref); s != nil {
		return s, nil
	}
	newest := make(map[integration.ChangeKey]*run.State)
	for _, s := range states {
		if s.Type == "session" || !run.ValidChangeID(s.ChangeID) || !refersTo(ref, s.ChangeID, recordedHandles(s)...) {
			continue
		}
		k := integration.ChangeKey{Repo: integration.RepoName(s), ChangeID: s.ChangeID}
		if prev, ok := newest[k]; !ok || createdBefore(prev, s) {
			newest[k] = s
		}
	}
	var lines []string
	for k, s := range newest {
		if len(newest) == 1 {
			return s, nil
		}
		lines = append(lines, changeName(k.ChangeID, handleOf(s))+" in "+k.Repo+", latest run "+s.ID)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("no run in this session is on a change %q: name a run ID, a valley handle or a change ID", ref)
	}
	return nil, ambiguous(ref, "this session", lines)
}

func init() {
	rootCmd.AddCommand(readyCmd)
}

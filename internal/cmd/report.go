package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/report"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "Report on klaus's runs from the session event logs",
}

var reportTimeCmd = &cobra.Command{
	Use:   "time [--since <duration>] [--repo <r>] [--current] [--json]",
	Short: "Report where agent wall-clock time goes",
	Long: `Reads the session event logs (events.jsonl in every session under
~/.klaus/sessions, or only the current session's with --current) and reports,
per repository and overall:

  RUNS        runs that started in the window, by dispatch reason
  SETUP       launch's work before the agent starts (fetch, worktree, pane)
  AGENT       the agents' own work, from pane start to finalize
  FINALIZE    finalize's work (salvage, push checks, data ref, cleanup)
  CHECKS      _pre-review and the build, test and lint commands agents ran,
              by kind; part of AGENT. A command line that also ran
              _pre-review counts without the pre-review's time
  WAIT        time a change had no run going, from the last end among its
              earlier runs to the next one's start: on review, CI or the
              operator; unknown after a run with no end
  APPROVAL    time from a PR's last run to its first approval
  FOLLOW-UPS  runs that continued a change (CI, review and conflict fixes,
              --pr, --change, budget resumes), per change
  CONSULT     klaus consult time; REVIEW, klaus review time
  COST        agent cost where the backend reports it (Claude)

Runs that share a change ID in a repo, or a PR, are one change, so a PR's
runs from before change IDs join its later ones, and a Change-Id copied into
another repo is another change. A run's repo is its repo_key: owner/repo,
valley:<name>, or the repo's short name; every name a run's events give its
repo counts, so a renamed repo's runs stay one change. Events from before
klaus recorded phases count a run's agent time from agent:started to its last
event, with no setup or finalize time, and a run with no recorded dispatch
reason counts as "unknown".

--since takes a Go duration or whole days or weeks (7d, 2w, 36h); 0 means all
time. --repo keeps one repo: a repo key, a valley's name for it, or its last
path component. --json prints the report as JSON, durations in milliseconds.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		sinceFlag, _ := cmd.Flags().GetString("since")
		repo, _ := cmd.Flags().GetString("repo")
		current, _ := cmd.Flags().GetBool("current")
		asJSON, _ := cmd.Flags().GetBool("json")

		window, err := report.ParseSince(sinceFlag)
		if err != nil {
			return err
		}
		now := time.Now()
		opts := report.Options{Until: now, Repo: repo}
		if window > 0 {
			opts.Since = now.Add(-window)
		}

		dirs, err := reportSessionDirs(current)
		if err != nil {
			return err
		}
		var sessions [][]event.Event
		for _, dir := range dirs {
			evts, err := event.NewLog(dir).Read()
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: reading %s: %v\n", event.NewLog(dir).Path(), err)
				continue
			}
			if evts != nil {
				sessions = append(sessions, evts)
			}
		}

		rep := report.Build(sessions, opts)
		out := cmd.OutOrStdout()
		if asJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(rep)
		}
		return rep.WriteText(out)
	},
}

// reportSessionDirs returns the session directories whose event logs the
// report reads: every session's, or the current one's.
func reportSessionDirs(current bool) ([]string, error) {
	if current {
		store, err := sessionStore()
		if err != nil {
			return nil, err
		}
		hds, ok := store.(*run.HomeDirStore)
		if !ok {
			return nil, fmt.Errorf("the current session keeps no event log")
		}
		return []string{hds.BaseDir()}, nil
	}
	root, err := run.SessionsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading sessions dir: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs, nil
}

func init() {
	reportTimeCmd.Flags().String("since", "7d", "Only runs, consults and reviews that started within this long ago (e.g. 7d, 2w, 36h; 0 for all time)")
	reportTimeCmd.Flags().String("repo", "", "Only this repo: owner/repo, valley:<name>, or a repo's name")
	reportTimeCmd.Flags().Bool("current", false, "Only the current session's events (default: every session's)")
	reportTimeCmd.Flags().Bool("json", false, "Print the report as JSON, durations in milliseconds")
	reportCmd.AddCommand(reportTimeCmd)
	rootCmd.AddCommand(reportCmd)
}

package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

var untrackCmd = &cobra.Command{
	Use:   "untrack <ref> [<ref>...]",
	Short: "Stop tracking PRs or valley changes on the dashboard",
	Long: `Removes tracked PRs or valley changes from the klaus dashboard and klaus
status. Only removes what was added via 'klaus track' — agent-created runs are
never removed.

A PR is named by its number. A valley change is named by its handle, its
full change ID, or <project>:<handle>; --repo names the project when the ref
doesn't. A change ref that names tracked changes in more than one project
removes none; name the project.

Examples:
  klaus untrack 405
  klaus untrack vpkz
  klaus untrack the-valley:vpkz
  klaus untrack --repo the-valley vpkz`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		repoFlag, _ := cmd.Flags().GetString("repo")
		store, err := sessionStore()
		if err != nil {
			return err
		}

		states, err := store.List()
		if err != nil {
			return err
		}

		for _, ref := range args {
			if project, id, ok := parseChangeRef(ref); ok {
				if project == "" {
					project = repoFlag
				}
				untrackChange(store, states, ref, project, id)
				continue
			}
			prNum := strings.TrimPrefix(ref, "#")
			found := false
			for _, s := range states {
				if s.Type != "track" {
					continue
				}
				if extractPRNumber(s) == prNum {
					if err := store.Delete(s.ID); err != nil {
						fmt.Fprintf(os.Stderr, "warning: failed to remove PR #%s: %v\n", prNum, err)
					} else {
						fmt.Printf("Stopped tracking PR #%s\n", prNum)
					}
					found = true
					break
				}
			}
			if !found {
				fmt.Fprintf(os.Stderr, "warning: no tracked PR #%s found\n", prNum)
			}
		}
		return nil
	},
}

// untrackChange removes the track record of the valley change id names, in
// project when it isn't "", saying what it did, or why it removed none.
func untrackChange(store run.StateStore, states []*run.State, ref, project, id string) {
	matches := trackedChanges(states, project, id)
	switch len(matches) {
	case 0:
		fmt.Fprintf(os.Stderr, "warning: no tracked change %s found\n", ref)
		return
	case 1:
	default:
		var names []string
		for _, s := range matches {
			names = append(names, s.Valley.Repo+":"+s.Valley.Handle)
		}
		fmt.Fprintf(os.Stderr, "warning: %s names tracked changes in more than one project (%s); name the project\n", ref, strings.Join(names, ", "))
		return
	}
	s := matches[0]
	if err := store.Delete(s.ID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to stop tracking change %s: %v\n", ref, err)
		return
	}
	fmt.Printf("Stopped tracking change %s (%s)\n", s.Valley.Handle, s.Valley.Repo)
}

func init() {
	untrackCmd.Flags().String("repo", "", "Project of a valley change named by its handle or change ID")
	rootCmd.AddCommand(untrackCmd)
}

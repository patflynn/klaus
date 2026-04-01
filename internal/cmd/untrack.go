package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var untrackCmd = &cobra.Command{
	Use:   "untrack <pr-number> [<pr-number>...]",
	Short: "Stop tracking PRs on the dashboard",
	Long: `Removes tracked PRs from the klaus dashboard. Only removes PRs that were
added via 'klaus track' — agent-created runs are never removed.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := sessionStore()
		if err != nil {
			return err
		}

		states, err := store.List()
		if err != nil {
			return err
		}

		for _, prNum := range args {
			prNum = strings.TrimPrefix(prNum, "#")
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

func init() {
	rootCmd.AddCommand(untrackCmd)
}

package cmd

import (
	"fmt"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

var targetCmd = &cobra.Command{
	Use:   "target [owner/repo]",
	Short: "Set or show the session-level default target repo",
	Long: `Sets a session-level default repo so that 'klaus launch' without --repo
uses this target. Useful when the coordinator session is not inside a git repo.

  klaus target owner/repo   Set the default target repo
  klaus target              Show the current target repo
  klaus target --clear      Remove the default target repo`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := sessionStore()
		if err != nil {
			return err
		}
		hds, ok := store.(*run.HomeDirStore)
		if !ok {
			return fmt.Errorf("target command requires a home-dir session store")
		}
		baseDir := hds.BaseDir()

		clear, _ := cmd.Flags().GetBool("clear")

		if clear {
			if err := run.ClearTarget(baseDir); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Target cleared.")
			return nil
		}

		if len(args) == 0 {
			// Show current target
			repo, err := run.LoadTarget(baseDir)
			if err != nil {
				return err
			}
			if repo == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "No target repo set.")
				fmt.Fprintln(cmd.OutOrStdout(), "Usage: klaus target owner/repo")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), repo)
			}
			return nil
		}

		// Validate the repo reference
		repoRef := args[0]
		owner, repo, _, err := git.ParseRepoRef(repoRef)
		if err != nil {
			return fmt.Errorf("invalid repo reference: %w", err)
		}

		// Normalize to owner/repo format
		normalized := owner + "/" + repo
		if err := run.SaveTarget(baseDir, normalized); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Target set to %s\n", normalized)
		return nil
	},
}

func init() {
	targetCmd.Flags().Bool("clear", false, "Remove the default target repo")
	rootCmd.AddCommand(targetCmd)
}

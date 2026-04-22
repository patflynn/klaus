package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/projectsync"
	"github.com/spf13/cobra"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Fetch and fast-forward every registered project",
	Long: `Synchronously fetches every project in ~/.klaus/projects.json and fast-forwards
its current branch to the upstream when the working tree is clean.

Projects with a dirty tree, diverged branch, detached HEAD, or missing upstream
are reported as skipped and left untouched. klaus never resets or force-updates
a working clone.

Exit code is 0 if every project succeeded (up-to-date / pulled / fetched-only)
or was intentionally skipped. A non-zero exit code means at least one project
hit an error (typically a fetch failure).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		reg, err := project.Load()
		if err != nil {
			return fmt.Errorf("loading project registry: %w", err)
		}
		if reg == nil || len(reg.Projects) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No projects registered. Use 'klaus project add' to register one.")
			return nil
		}

		results := projectsync.Sync(cmd.Context(), reg, git.NewExecClient())
		// Also append to the log file so CLI invocations appear there.
		projectsync.WriteLog("cli", results)

		writeSyncTable(cmd.OutOrStdout(), results)

		for _, r := range results {
			if r.Status == projectsync.StatusError {
				return fmt.Errorf("one or more projects failed to sync")
			}
		}
		return nil
	},
}

func writeSyncTable(w io.Writer, results []projectsync.SyncResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tSTATUS\tBRANCH\tDETAIL")
	for _, r := range results {
		status := string(r.Status)
		// Include the skip reason in the STATUS column for parity with the
		// documented output format (e.g. "skipped: dirty tree").
		if r.Status == projectsync.StatusSkipped && r.Detail != "" {
			status = string(r.Status) + ": " + r.Detail
		} else if r.Status == projectsync.StatusError && r.Detail != "" {
			status = string(r.Status) + ": " + r.Detail
		}
		detail := r.Detail
		if r.Status == projectsync.StatusSkipped || r.Status == projectsync.StatusError {
			detail = "" // already rolled into status
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, status, r.Branch, detail)
	}
	tw.Flush()
}

func init() {
	rootCmd.AddCommand(syncCmd)
}

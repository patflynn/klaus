package cmd

import (
	"fmt"
	"os"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/spf13/cobra"
)

var pushLogPush bool

var pushLogCmd = &cobra.Command{
	Use:   "push-log <run-id>",
	Short: "Store a log held back for sensitivity on the data ref",
	Long: `Commits a run's log to the data ref after the sensitivity scan held it
back at finalize. Use after reviewing the log and confirming it's safe.

The commit goes to the data ref in the run's local clone. It is pushed to
origin only with --push, or when push_data_ref is true in config. A pushed
data ref is world-readable on a public repo, and the push sends the ref's
whole history, not just this log.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		store, err := sessionStore()
		if err != nil {
			return err
		}
		state, err := store.Load(id)
		if err != nil {
			return fmt.Errorf("no run found with id: %s", id)
		}

		// Same repo _finalize synced to, so replay finds it there.
		var root string
		if state.CloneDir != nil {
			root = *state.CloneDir
		} else if root, err = git.RepoRoot(); err != nil {
			return fmt.Errorf("not inside a git repository")
		}

		cfg, err := config.Load(root)
		if err != nil {
			return err
		}

		if state.LogFile == nil {
			return fmt.Errorf("no log file for run %s", id)
		}

		if _, err := os.Stat(*state.LogFile); err != nil {
			return fmt.Errorf("log file not found: %s", *state.LogFile)
		}

		ctx := cmd.Context()
		gitClient := git.NewExecClient()

		stateFile := store.StateDir() + "/" + id + ".json"
		files := map[string]string{
			"runs/" + id + ".json":  stateFile,
			"logs/" + id + ".jsonl": *state.LogFile,
		}

		if err := gitClient.SyncToDataRef(ctx, root, cfg.DataRef, "Run "+id, files); err != nil {
			return fmt.Errorf("syncing to data ref: %w", err)
		}
		fmt.Printf("Committed log for %s to %s in %s (sensitivity check bypassed).\n", id, cfg.DataRef, root)

		if !pushLogPush && !cfg.PushDataRef {
			fmt.Printf("Not pushed: push_data_ref is false, so %s stays local. Pass --push to publish it to origin.\n", cfg.DataRef)
			return nil
		}

		if err := gitClient.PushDataRef(ctx, root, cfg.DataRef); err != nil {
			return fmt.Errorf("pushing %s to origin (the local commit is kept): %w", cfg.DataRef, err)
		}
		fmt.Printf("Pushed %s to origin.\n", cfg.DataRef)
		return nil
	},
}

func init() {
	pushLogCmd.Flags().BoolVar(&pushLogPush, "push", false, "also push the data ref to origin (default: push only when push_data_ref is true)")
	rootCmd.AddCommand(pushLogCmd)
}

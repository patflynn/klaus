package cmd

import (
	"fmt"
	"os"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/spf13/cobra"
)

var pushLogCmd = &cobra.Command{
	Use:   "push-log <run-id>",
	Short: "Force-push a previously skipped log to the data ref",
	Long: `Pushes a log file that was previously skipped due to sensitivity
warnings. Use after reviewing the log and confirming it's safe.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		root, err := git.RepoRoot()
		if err != nil {
			return fmt.Errorf("not inside a git repository")
		}

		cfg, err := config.Load(root)
		if err != nil {
			return err
		}

		state, store, err := loadStateFromEnvOrAll(id)
		if err != nil {
			return err
		}

		if state.LogFile == nil {
			return fmt.Errorf("no log file for run %s", id)
		}

		if _, err := os.Stat(*state.LogFile); err != nil {
			return fmt.Errorf("log file not found: %s", *state.LogFile)
		}

		fmt.Printf("Force-pushing log for %s (bypassing sensitivity check)...\n", id)

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

		// Push to remote
		if err := gitClient.PushDataRef(ctx, root, cfg.DataRef); err != nil {
			fmt.Printf("  warning: push to remote failed: %v\n", err)
		}

		fmt.Println("Done.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(pushLogCmd)
}

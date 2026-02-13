package cmd

import (
	"fmt"
	"os"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
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

		commonDir, err := git.CommonDir()
		if err != nil {
			return err
		}

		cfg, err := config.Load(root)
		if err != nil {
			return err
		}

		state, err := run.Load(commonDir, id)
		if err != nil {
			return fmt.Errorf("no run found with id: %s", id)
		}

		if state.LogFile == nil {
			return fmt.Errorf("no log file for run %s", id)
		}

		if _, err := os.Stat(*state.LogFile); err != nil {
			return fmt.Errorf("log file not found: %s", *state.LogFile)
		}

		fmt.Printf("Force-pushing log for %s (bypassing sensitivity check)...\n", id)

		stateFile := run.StateDir(commonDir) + "/" + id + ".json"
		files := map[string]string{
			"runs/" + id + ".json":  stateFile,
			"logs/" + id + ".jsonl": *state.LogFile,
		}

		if err := git.SyncToDataRef(root, cfg.DataRef, "Run "+id, files); err != nil {
			return fmt.Errorf("syncing to data ref: %w", err)
		}

		// Push to remote
		if err := git.PushDataRef(root, cfg.DataRef); err != nil {
			fmt.Printf("  warning: push to remote failed: %v\n", err)
		}

		fmt.Println("Done.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(pushLogCmd)
}

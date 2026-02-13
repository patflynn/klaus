package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/stream"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs <run-id>",
	Short: "View agent logs",
	Long: `Shows agent output. By default shows the live tmux pane if running,
or replays from saved log if finished.

Modes:
  --live     Show live pane or replay from log (default)
  --replay   Re-format the saved JSONL log
  --raw      Dump the raw JSONL log file`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		raw, _ := cmd.Flags().GetBool("raw")
		replay, _ := cmd.Flags().GetBool("replay")

		commonDir, err := git.CommonDir()
		if err != nil {
			return fmt.Errorf("not inside a git repository")
		}

		state, err := run.Load(commonDir, id)
		if err != nil {
			return fmt.Errorf("no run found with id: %s", id)
		}

		if raw {
			return showRawLog(state)
		}
		if replay {
			return replayLog(state)
		}
		return showLive(state)
	},
}

func showRawLog(s *run.State) error {
	if s.LogFile == nil {
		return fmt.Errorf("no log file for run %s", s.ID)
	}
	f, err := os.Open(*s.LogFile)
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}
	defer f.Close()
	_, err = io.Copy(os.Stdout, f)
	return err
}

func replayLog(s *run.State) error {
	if s.LogFile == nil {
		return fmt.Errorf("no log file for run %s", s.ID)
	}
	f, err := os.Open(*s.LogFile)
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}
	defer f.Close()
	return stream.FormatStream(f, os.Stdout)
}

func showLive(s *run.State) error {
	// Try live tmux pane first
	if s.TmuxPane != nil && tmux.PaneExists(*s.TmuxPane) {
		output, err := tmux.CapturePane(*s.TmuxPane, 500)
		if err != nil {
			return fmt.Errorf("capturing pane: %w", err)
		}
		fmt.Print(output)
		return nil
	}

	// Fall back to replay
	if s.LogFile != nil {
		return replayLog(s)
	}

	fmt.Printf("No live pane or log file available for run %s.\n", s.ID)
	return nil
}

func init() {
	logsCmd.Flags().Bool("raw", false, "Dump raw JSONL log")
	logsCmd.Flags().Bool("replay", false, "Re-format saved log through formatter")
	rootCmd.AddCommand(logsCmd)
}

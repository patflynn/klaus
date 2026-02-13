package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/scan"
	"github.com/patflynn/klaus/internal/stream"
	"github.com/spf13/cobra"
)

var formatStreamCmd = &cobra.Command{
	Use:    "_format-stream",
	Short:  "Format Claude JSONL stream from stdin",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return stream.FormatStream(os.Stdin, os.Stdout)
	},
}

var finalizeCmd = &cobra.Command{
	Use:    "_finalize <run-id>",
	Short:  "Finalize a completed run",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		commonDir, err := git.CommonDir()
		if err != nil {
			return err
		}

		state, err := run.Load(commonDir, id)
		if err != nil {
			return nil // silently ignore if state not found
		}

		// Parse log for cost/duration/PR URL
		if state.LogFile != nil {
			if err := finalizeFromLog(commonDir, state); err != nil {
				fmt.Fprintf(os.Stderr, "warning: finalize: %v\n", err)
			}
		}

		// Sync to data ref
		root, err := git.RepoRoot()
		if err != nil {
			return nil
		}

		cfg, err := config.Load(root)
		if err != nil {
			return nil
		}

		syncRunToDataRef(root, commonDir, cfg.DataRef, state)
		return nil
	},
}

func finalizeFromLog(commonDir string, state *run.State) error {
	f, err := os.Open(*state.LogFile)
	if err != nil {
		return err
	}
	defer f.Close()

	// Scan for result event with cost/duration
	scanner := json.NewDecoder(f)
	for scanner.More() {
		var ev struct {
			Type         string  `json:"type"`
			TotalCostUSD float64 `json:"total_cost_usd"`
			DurationMS   int64   `json:"duration_ms"`
			Message      *struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := scanner.Decode(&ev); err != nil {
			continue
		}

		switch ev.Type {
		case "result":
			if ev.TotalCostUSD > 0 {
				state.CostUSD = &ev.TotalCostUSD
			}
			if ev.DurationMS > 0 {
				state.DurationMS = &ev.DurationMS
			}
		case "assistant":
			if ev.Message != nil {
				for _, block := range ev.Message.Content {
					if block.Type == "text" {
						if url := extractPRURL(block.Text); url != "" {
							state.PRURL = &url
						}
					}
				}
			}
		}
	}

	return run.Save(commonDir, state)
}

func extractPRURL(text string) string {
	// Look for GitHub PR URLs
	for _, word := range strings.Fields(text) {
		// Clean trailing punctuation
		word = strings.TrimRight(word, ".,;:!?)")
		if strings.Contains(word, "github.com/") && strings.Contains(word, "/pull/") {
			return word
		}
	}
	return ""
}

func syncRunToDataRef(root, commonDir, dataRef string, state *run.State) {
	stateFile := run.StateDir(commonDir) + "/" + state.ID + ".json"
	files := map[string]string{
		"runs/" + state.ID + ".json": stateFile,
	}

	// Check log sensitivity before including
	if state.LogFile != nil {
		logF, err := os.Open(*state.LogFile)
		if err == nil {
			findings := scan.CheckSensitivity(logF)
			logF.Close()

			if len(findings) == 0 {
				files["logs/"+state.ID+".jsonl"] = *state.LogFile
			} else {
				fmt.Fprintf(os.Stderr, "warning: skipping log push for %s: potentially sensitive data detected\n", state.ID)
				for _, f := range findings {
					fmt.Fprintf(os.Stderr, "  - %s\n", f.Category)
				}
				fmt.Fprintf(os.Stderr, "  Use 'klaus push-log %s' to push manually.\n", state.ID)
			}
		}
	}

	if err := git.SyncToDataRef(root, dataRef, "Run "+state.ID, files); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync to data ref: %v\n", err)
		return
	}

	if err := git.PushDataRef(root, dataRef); err != nil {
		// Silently ignore push failures (no remote, etc.)
	}
}

func init() {
	rootCmd.AddCommand(formatStreamCmd)
	rootCmd.AddCommand(finalizeCmd)
}

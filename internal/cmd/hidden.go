package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

		store, err := sessionStore()
		if err != nil {
			return nil // silently ignore if session not set
		}

		state, err := store.Load(id)
		if err != nil {
			return nil // silently ignore if state not found
		}

		// Parse log for cost/duration/PR URL
		if state.LogFile != nil {
			if err := finalizeFromLog(store, state); err != nil {
				fmt.Fprintf(os.Stderr, "warning: finalize: %v\n", err)
			}
		}

		// Sync to data ref — still uses the git repo
		root, err := git.RepoRoot()
		if err != nil {
			return nil
		}

		cfg, err := config.Load(root)
		if err != nil {
			return nil
		}

		syncRunToDataRef(root, store, cfg.DataRef, state)
		return nil
	},
}

func finalizeFromLog(store run.StateStore, state *run.State) error {
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

	return store.Save(state)
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

var prURLRegex = regexp.MustCompile(`/pull/(\d+)`)

// extractPRNumberFromURL extracts the PR number from a GitHub PR URL.
// For example, "https://github.com/owner/repo/pull/123" returns "123".
func extractPRNumberFromURL(prURL string) string {
	matches := prURLRegex.FindStringSubmatch(prURL)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

var autoWatchCmd = &cobra.Command{
	Use:    "_auto-watch <run-id>",
	Short:  "Auto-launch watch agent if run created a PR",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		store, err := sessionStore()
		if err != nil {
			return fmt.Errorf("auto-watch: %w", err)
		}

		state, err := store.Load(filepath.Base(id))
		if err != nil {
			return fmt.Errorf("auto-watch: failed to load state for run %s: %w", id, err)
		}

		// No PR created — nothing to do
		if state.PRURL == nil || *state.PRURL == "" {
			return nil
		}

		prNumber := extractPRNumberFromURL(*state.PRURL)
		if prNumber == "" {
			fmt.Fprintf(os.Stderr, "warning: auto-watch: could not extract PR number from %s\n", *state.PRURL)
			return nil
		}

		// Determine git root for worktree/branch cleanup
		gitRoot, err := git.RepoRoot()
		if err != nil {
			return fmt.Errorf("auto-watch: not inside a git repository")
		}
		if state.CloneDir != nil {
			gitRoot = *state.CloneDir
		}

		// Move to a directory that survives worktree removal
		watchDir := gitRoot
		if err := os.Chdir(watchDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: auto-watch: chdir to %s: %v\n", watchDir, err)
		}

		// Clean up agent worktree and branch so watch can use the PR branch
		if state.Worktree != "" {
			if err := git.WorktreeRemove(gitRoot, state.Worktree); err != nil {
				fmt.Fprintf(os.Stderr, "warning: auto-watch: removing worktree: %v\n", err)
			}
		}
		if state.Branch != "" {
			if err := git.BranchDelete(gitRoot, state.Branch); err != nil {
				fmt.Fprintf(os.Stderr, "warning: auto-watch: deleting branch: %v\n", err)
			}
		}

		fmt.Printf("\nAgent created PR #%s — launching watch agent...\n", prNumber)

		proc := exec.Command("klaus", "watch", prNumber)
		proc.Dir = watchDir
		proc.Stdout = os.Stdout
		proc.Stderr = os.Stderr
		proc.Stdin = os.Stdin
		if err := proc.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "error: auto-watch: launching watch: %v\n", err)
		}

		return nil
	},
}

func syncRunToDataRef(root string, store run.StateStore, dataRef string, state *run.State) {
	stateFile := store.StateDir() + "/" + state.ID + ".json"
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
	rootCmd.AddCommand(autoWatchCmd)
}

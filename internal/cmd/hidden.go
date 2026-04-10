package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/event"
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

		// Emit events for completed run
		if hds, ok := store.(*run.HomeDirStore); ok {
			baseDir := hds.BaseDir()

			completedData := map[string]interface{}{"id": id}
			if state.CostUSD != nil {
				completedData["cost_usd"] = *state.CostUSD
			}
			if state.DurationMS != nil {
				completedData["duration_ms"] = *state.DurationMS
			}
			emitEvent(baseDir, id, event.AgentCompleted, completedData)

			if state.PRURL != nil && *state.PRURL != "" {
				prNum := extractPRNumberFromURL(*state.PRURL)
				emitEvent(baseDir, id, event.AgentPRCreated, map[string]interface{}{
					"id":        id,
					"pr_url":    *state.PRURL,
					"pr_number": prNum,
				})
			}
		}

		// Sync to data ref — use the target repo's clone dir if available,
		// otherwise fall back to the current git repo.
		var syncRoot string
		if state.CloneDir != nil {
			syncRoot = *state.CloneDir
		} else {
			syncRoot, err = git.RepoRoot()
			if err != nil {
				return nil
			}
		}

		cfg, err := config.Load(syncRoot)
		if err != nil {
			return nil
		}

		syncRunToDataRef(syncRoot, store, cfg.DataRef, state)

		cleanupWorktree(store, state)

		return nil
	},
}

// cleanupWorktree removes the agent's worktree and local branch after
// completion. The state file and logs are preserved. It is idempotent —
// if the worktree is already gone, the state is still cleared.
func cleanupWorktree(store run.StateStore, state *run.State) {
	if state.Worktree == "" {
		return
	}
	gitRoot := ""
	if state.CloneDir != nil {
		gitRoot = *state.CloneDir
	} else {
		gitRoot, _ = git.RepoRoot()
	}
	if gitRoot == "" {
		return
	}
	if err := git.WorktreeRemove(gitRoot, state.Worktree); err != nil {
		fmt.Fprintf(os.Stderr, "warning: worktree cleanup: %v\n", err)
	}
	if state.Branch != "" {
		if err := git.BranchDelete(gitRoot, state.Branch); err != nil {
			slog.Warn("failed to delete branch during cleanup", "branch", state.Branch, "err", err)
		}
	}
	state.Worktree = ""
	if err := store.Save(state); err != nil {
		slog.Warn("failed to save state after worktree cleanup", "id", state.ID, "err", err)
	}
}

func finalizeFromLog(store run.StateStore, state *run.State) error {
	f, err := os.Open(*state.LogFile)
	if err != nil {
		return err
	}
	defer f.Close()

	// Use line-by-line scanning for robust JSONL parsing.
	// json.NewDecoder can corrupt its internal state on malformed lines,
	// causing subsequent events to be silently skipped.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev struct {
			Type         string  `json:"type"`
			TotalCostUSD float64 `json:"total_cost_usd"`
			DurationMS   int64   `json:"duration_ms"`
			Message      *struct {
				Content []struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Content string `json:"content"`
				} `json:"content"`
			} `json:"message"`
			// Top-level content for tool_result events
			Content string `json:"content"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
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
		default:
			// Handle tool_result and other event types that may
			// contain the PR URL (e.g. gh pr create output).
			if ev.Content != "" {
				if url := extractPRURL(ev.Content); url != "" {
					state.PRURL = &url
				}
			}
			if ev.Message != nil {
				for _, block := range ev.Message.Content {
					text := block.Text
					if text == "" {
						text = block.Content
					}
					if text != "" {
						if url := extractPRURL(text); url != "" {
							state.PRURL = &url
						}
					}
				}
			}
		}
	}

	return store.Save(state)
}

// prURLExtractRegex matches GitHub PR URLs in free-form text, including
// inside markdown links, angle brackets, or adjacent punctuation.
var prURLExtractRegex = regexp.MustCompile(`https?://github\.com/[^\s"<>\]]+/pull/\d+`)

func extractPRURL(text string) string {
	return prURLExtractRegex.FindString(text)
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

// ExtractClaudeSessionID parses a Claude stream-json JSONL log file and
// returns the session_id from the "result" event. Returns empty string if
// not found or on any error.
func ExtractClaudeSessionID(logPath string) string {
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	var ev struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Type == "result" && ev.SessionID != "" {
			return ev.SessionID
		}
	}
	return ""
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
}

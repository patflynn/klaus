package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/review"
	"github.com/spf13/cobra"
)

var preReviewCmd = &cobra.Command{
	Use:    "_pre-review",
	Short:  "Run pre-PR linting and peer review",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) (err error) {
		dir, _ := cmd.Flags().GetString("dir")
		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}
		}

		cfg, err := config.Load(dir)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}

		if !cfg.PreReviewEnabled() {
			fmt.Println("Pre-review is disabled in config.")
			return nil
		}

		check := startPreReviewCheck()
		defer func() {
			if err != nil {
				check.finish(false, map[string]interface{}{"error": errorLine(err)})
			}
		}()

		fmt.Println("=== Pre-PR Review ===")
		fmt.Println()

		// Run linters
		linters := cfg.PreReviewLinters()
		lintFailed := false
		if len(linters) > 0 {
			fmt.Println("Linters:")
			results, err := review.RunLinters(dir, linters)
			if err != nil {
				return fmt.Errorf("running linters: %w", err)
			}
			for _, r := range results {
				if r.Passed {
					fmt.Printf("  ✓ %s\n", r.Command)
				} else {
					lintFailed = true
					// Count issues from output lines
					lines := strings.Split(strings.TrimSpace(r.Output), "\n")
					count := 0
					for _, l := range lines {
						if strings.TrimSpace(l) != "" {
							count++
						}
					}
					fmt.Printf("  ✗ %s (%d issues)\n", r.Command, count)
					// Print up to 10 lines of output indented
					for i, line := range lines {
						if i >= 10 {
							fmt.Printf("    ... and %d more\n", count-10)
							break
						}
						if strings.TrimSpace(line) != "" {
							fmt.Printf("    %s\n", line)
						}
					}
				}
			}
			fmt.Println()
		}

		// Run peer review with a different model family than the worker's.
		authorName := os.Getenv("KLAUS_BACKEND")
		if authorName == "" {
			selected, err := resolveAgentBackend(cmd, cfg)
			if err != nil {
				return err
			}
			authorName = string(selected)
		}
		author, err := backend.Parse(authorName)
		if err != nil {
			return err
		}
		kind, model, err := review.ChooseReviewer(author, cfg)
		if err != nil {
			return err
		}
		fmt.Printf("Peer Review (reviewer: %s %s; author: %s):\n", kind, modelLabel(model), author)
		check.data["reviewer"] = string(kind)
		check.data["lint_ms"] = time.Since(check.start).Milliseconds()
		reviewStart := time.Now()
		result, err := review.ReviewDiff(dir, review.ReviewConfig{
			Backend:      string(kind),
			Model:        model,
			MaxFixRounds: cfg.PreReviewMaxFixRounds(),
		}, cfg.DefaultBranch)
		if err != nil {
			return fmt.Errorf("running peer review: %w", err)
		}
		printFindings(os.Stdout, result.Findings)
		fmt.Println()

		// Determine if we should block
		blockOn := cfg.PreReviewBlockOn()
		blockSeverities := severitiesAtOrAbove(blockOn)
		blockCount := 0
		for _, f := range result.Findings {
			if blockSeverities[f.Severity] {
				blockCount++
			}
		}

		// os.Exit skips deferred calls, so the check ends first.
		check.finish(!lintFailed && blockCount == 0, map[string]interface{}{
			"review_ms":   time.Since(reviewStart).Milliseconds(),
			"findings":    len(result.Findings),
			"blocking":    blockCount,
			"lint_failed": lintFailed,
		})

		if lintFailed {
			fmt.Println("Lint failures detected — PR creation should be blocked.")
			os.Exit(1)
		}

		if blockCount > 0 {
			label := "finding"
			if blockCount > 1 {
				label = "findings"
			}
			fmt.Printf("%d %s %s — PR creation should be blocked.\n", blockCount, blockOn+"+", label)
			os.Exit(1)
		}

		fmt.Println("All checks passed.")
		return nil
	},
}

// preReviewCheck reports a _pre-review run as a check of the run whose pane
// it runs in (runIDEnv): check:started when it starts, check:completed when
// it finishes. Outside a run's pane it reports nothing.
type preReviewCheck struct {
	run     *paneRun
	start   time.Time
	data    map[string]interface{}
	started bool
	done    bool
}

func startPreReviewCheck() *preReviewCheck {
	c := &preReviewCheck{run: currentPaneRun(), start: time.Now()}
	c.data = map[string]interface{}{"kind": event.CheckPreReview, "command": "klaus _pre-review", "source": "pre-review", "started_at": eventTime(c.start)}
	if c.run != nil && c.run.state != nil {
		c.run.emit(event.CheckStarted, c.data)
		c.started = true
	}
	return c
}

// finish emits check:completed, once, with extra added to the check's data.
// A run launch hadn't saved yet when the check started gets both events now,
// once launch saves it or, after paneRunSaveWait, under the run's ID alone.
func (c *preReviewCheck) finish(success bool, extra map[string]interface{}) {
	if c.done || c.run == nil {
		return
	}
	c.done = true
	if !c.started {
		c.run.awaitState()
		c.run.emit(event.CheckStarted, c.data)
	}
	end := time.Now()
	data := map[string]interface{}{"ended_at": eventTime(end), "duration_ms": end.Sub(c.start).Milliseconds(), "success": success}
	for k, v := range c.data {
		data[k] = v
	}
	for k, v := range extra {
		data[k] = v
	}
	c.run.emit(event.CheckCompleted, data)
}

func modelLabel(model string) string {
	if model == "" {
		return "(default model)"
	}
	return model
}

func printFindings(w io.Writer, findings []review.Finding) {
	if len(findings) == 0 {
		fmt.Fprintln(w, "  No issues found.")
		return
	}
	for _, f := range findings {
		sev := strings.ToUpper(f.Severity)
		if f.Line > 0 {
			fmt.Fprintf(w, "  %-8s %s:%d — %s\n", sev, f.File, f.Line, f.Description)
		} else {
			fmt.Fprintf(w, "  %-8s %s — %s\n", sev, f.File, f.Description)
		}
	}
}

// severitiesAtOrAbove returns a set of severity levels at or above the given level.
func severitiesAtOrAbove(level string) map[string]bool {
	order := []string{"low", "medium", "high", "critical"}
	result := make(map[string]bool)
	found := false
	for _, s := range order {
		if s == strings.ToLower(level) {
			found = true
		}
		if found {
			result[s] = true
		}
	}
	// If level not recognized, block on critical only
	if !found {
		result["critical"] = true
	}
	return result
}

func init() {
	preReviewCmd.Flags().String("dir", "", "Worktree directory to review (defaults to current directory)")
	rootCmd.AddCommand(preReviewCmd)
}

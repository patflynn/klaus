package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/review"
	"github.com/spf13/cobra"
)

var preReviewCmd = &cobra.Command{
	Use:    "_pre-review",
	Short:  "Run pre-PR linting and peer review",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
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
		result := &review.ReviewResult{}
		kind, model, err := review.ChooseReviewer(author, cfg)
		switch {
		case errors.Is(err, review.ErrAgyReviewer): // only agy available; don't block agy-only setups
			fmt.Printf("Peer Review: skipped (%v)\n\n", err)
		case err != nil:
			return err
		default:
			fmt.Printf("Peer Review (reviewer: %s %s; author: %s):\n", kind, modelLabel(model), author)
			result, err = review.ReviewDiff(dir, review.ReviewConfig{
				Backend:      string(kind),
				Model:        model,
				MaxFixRounds: cfg.PreReviewMaxFixRounds(),
			}, cfg.DefaultBranch)
			if err != nil {
				return fmt.Errorf("running peer review: %w", err)
			}
			printFindings(os.Stdout, result.Findings)
			fmt.Println()
		}

		// Determine if we should block
		blockOn := cfg.PreReviewBlockOn()
		blockSeverities := severitiesAtOrAbove(blockOn)
		blockCount := 0
		for _, f := range result.Findings {
			if blockSeverities[f.Severity] {
				blockCount++
			}
		}

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

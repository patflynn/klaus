package cmd

import (
	"fmt"
	"os"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show all runs and their current state",
	RunE: func(cmd *cobra.Command, args []string) error {
		commonDir, err := git.CommonDir()
		if err != nil {
			return fmt.Errorf("not inside a git repository")
		}

		states, err := run.List(commonDir)
		if err != nil {
			return err
		}

		if len(states) == 0 {
			fmt.Println("No runs found.")
			return nil
		}

		fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-6s  %-30s  %s\n",
			"RUN ID", "STATUS", "COST", "ISSUE", "PR", "PROMPT")
		fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-6s  %-30s  %s\n",
			"------", "------", "----", "-----", "--", "------")

		for _, s := range states {
			status := determineStatus(s)
			cost := formatCost(s)
			issue := "-"
			if s.Issue != nil {
				issue = *s.Issue
			}
			pr := formatPR(s)
			prompt := truncate(s.Prompt, 40)

			fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-6s  %-30s  %s\n",
				s.ID, status, cost, issue, pr, prompt)
		}

		return nil
	},
}

func determineStatus(s *run.State) string {
	if s.Type == "session" {
		if _, err := os.Stat(s.Worktree); err == nil {
			return "active"
		}
		return "ended"
	}

	if s.TmuxPane != nil && tmux.PaneExists(*s.TmuxPane) {
		return "running"
	}

	if s.PRURL != nil {
		return "pr-created"
	}
	return "exited"
}

func formatCost(s *run.State) string {
	if s.CostUSD != nil {
		return fmt.Sprintf("$%.2f", *s.CostUSD)
	}
	if s.Budget != nil {
		return fmt.Sprintf("<$%s", *s.Budget)
	}
	return "-"
}

func formatPR(s *run.State) string {
	if s.PRURL == nil {
		return "-"
	}
	url := *s.PRURL
	// Extract PR number from URL
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '/' {
			return "#" + url[i+1:]
		}
	}
	return url
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

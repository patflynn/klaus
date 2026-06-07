package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

var approveCmd = &cobra.Command{
	Use:   "approve <pr-number> [<pr-number>...]",
	Short: "Approve PRs for merging",
	Long: `Marks PRs as approved so they can be merged with 'klaus merge'.

Accepts PR numbers, or use --run to approve by run ID.
Use --all to approve all merge-ready PRs.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		runID, _ := cmd.Flags().GetString("run")
		all, _ := cmd.Flags().GetBool("all")

		if !all && runID == "" && len(args) == 0 {
			return fmt.Errorf("specify PR numbers, --run <id>, or --all")
		}

		store, err := sessionStore()
		if err != nil {
			return err
		}
		states, err := store.List()
		if err != nil {
			return err
		}

		if all {
			return approveAll(states, store)
		}

		if runID != "" {
			return approveByRunID(runID, store)
		}

		return approveByPRNumbers(args, states, store)
	},
}

func approveAll(states []*run.State, store run.StateStore) error {
	count := 0
	for _, s := range states {
		if s.PRURL == nil || s.MergedAt != nil {
			continue
		}
		if s.Approved != nil && *s.Approved {
			continue
		}
		if err := markApproved(s, store); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to approve run %s: %v\n", s.ID, err)
			continue
		}
		prNum := extractPRNumber(s)
		fmt.Printf("Approved PR #%s (run %s)\n", prNum, shortID(s.ID))
		count++
	}
	if count == 0 {
		fmt.Println("No unapproved PRs found.")
	}
	return nil
}

func approveByRunID(id string, store run.StateStore) error {
	state, err := store.Load(id)
	if err != nil {
		return fmt.Errorf("run %s not found: %w", id, err)
	}
	if err := markApproved(state, store); err != nil {
		return err
	}
	prNum := extractPRNumber(state)
	if prNum == "" {
		prNum = "(no PR)"
	}
	fmt.Printf("Approved PR #%s (run %s)\n", prNum, shortID(state.ID))
	return nil
}

func approveByPRNumbers(prNumbers []string, states []*run.State, store run.StateStore) error {
	for _, prNum := range prNumbers {
		state, st, err := findRunByPR(prNum, states, store)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: no run found for PR #%s: %v\n", prNum, err)
			continue
		}
		if err := markApproved(state, st); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to approve PR #%s: %v\n", prNum, err)
			continue
		}
		fmt.Printf("Approved PR #%s (run %s)\n", prNum, shortID(state.ID))
	}
	return nil
}

func markApproved(s *run.State, store run.StateStore) error {
	approved := true
	s.Approved = &approved
	now := time.Now().UTC().Format(time.RFC3339)
	s.ApprovedAt = &now
	if store != nil {
		if err := store.Save(s); err != nil {
			return err
		}
	}
	emitApprovalChanged(store, s)
	return nil
}

// emitApprovalChanged writes a PRApprovalChanged event to the session event
// log so the dashboard FSM wakes without waiting for an unrelated GitHub
// webhook. The event is informational — failures are logged via stderr but
// do not fail the approve command, since the state itself is already
// persisted on disk and the dashboard's polling fallback (when enabled) will
// pick up the change on the next tick.
func emitApprovalChanged(store run.StateStore, s *run.State) {
	hds, ok := store.(*run.HomeDirStore)
	if !ok {
		return
	}
	data := map[string]interface{}{}
	if pr := extractPRNumber(s); pr != "" {
		data["pr_number"] = pr
	}
	if s.PRURL != nil {
		data["pr_url"] = *s.PRURL
	}
	data["approved"] = true
	log := event.NewLog(hds.BaseDir())
	if err := log.Emit(event.New(s.ID, event.PRApprovalChanged, data)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to emit approval event: %v\n", err)
	}
}

// findRunByPR finds a run state matching a PR number in the given store.
func findRunByPR(prNumber string, states []*run.State, store run.StateStore) (*run.State, run.StateStore, error) {
	prNumber = strings.TrimPrefix(prNumber, "#")
	for _, s := range states {
		if extractPRNumber(s) == prNumber {
			return s, store, nil
		}
	}
	return nil, nil, fmt.Errorf("no run with PR #%s", prNumber)
}

// shortID returns a truncated run ID for display (last 4 chars).
func shortID(id string) string {
	if len(id) <= 4 {
		return id
	}
	parts := strings.Split(id, "-")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return id
}

func init() {
	approveCmd.Flags().String("run", "", "Approve by run ID instead of PR number")
	approveCmd.Flags().Bool("all", false, "Approve all merge-ready PRs")
	rootCmd.AddCommand(approveCmd)
}

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

const markerFile = "notifications-marker"

var notificationsCmd = &cobra.Command{
	Use:   "notifications",
	Short: "Show recent events and actionable items",
	Long: `Displays events since the last check, grouped into actionable summaries.

By default shows only events since the last time notifications was run.
Use --all to show all events, or --json for machine-readable output.

A run that needed attention is left out of the summary once a run of its
change took its pushed work on and is running or finished, as klaus status
shows it superseded: that run carries the attention, and its own
agent:needs-attention says so if it ended needing attention or failing.
--json lists every event.`,
	Aliases: []string{"notif"},
	RunE: func(cmd *cobra.Command, args []string) error {
		showAll, _ := cmd.Flags().GetBool("all")
		jsonOut, _ := cmd.Flags().GetBool("json")

		store, err := sessionStore()
		if err != nil {
			return err
		}

		baseDir := filepath.Dir(store.StateDir())
		log := event.NewLog(baseDir)

		var events []event.Event
		if showAll {
			events, err = log.Read()
		} else {
			marker := loadMarker(baseDir)
			var newMarker string
			events, newMarker, err = log.ReadSince(marker)
			if err == nil {
				saveMarker(baseDir, newMarker)
			}
		}
		if err != nil {
			return fmt.Errorf("reading events: %w", err)
		}

		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(events)
		}

		if len(events) == 0 {
			fmt.Println("No new notifications.")
			return nil
		}

		states, err := store.List()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: reading runs: %v\n", err)
		}
		td := run.TmuxDepsFor(cmd.Context(), tmux.NewExecClient())
		printSummary(events, supersededRuns(states, func(s *run.State) bool { return s.IsAgentRunningWith(td) }))
		return nil
	},
}

// printSummary prints the events' actionable summary. A needs-attention
// event of a run in superseded is left out: a later run took its work on.
func printSummary(events []event.Event, superseded map[string]bool) {
	var (
		prsReady       []string
		needsAttention []string
		completed      []completedInfo
		prCreated      []string
		branchPushed   []runNote
		changeReady    []string
		changeDraft    []runNote
		readyRuns      = make(map[string]bool) // runs listed under changeReady or changeDraft
		readied        = make(map[string]bool) // runs listed under changeReady
		ciFailed       []string
		ciPassed       []string
		prMerged       []string
		// A valley change's transitions, as its change:* events report them.
		changeTurnedBack []string // stale, rejected or failed
		changeApproved   []string
		changeVersions   []string
		changeLanded     []string
		changeWithdrawn  []string
	)

	for _, evt := range events {
		id := shortID(evt.RunID)
		switch evt.Type {
		case event.AgentCompleted:
			cost, ok := evt.Data["cost_usd"].(float64)
			if !ok {
				cost = 0
			}
			completed = append(completed, completedInfo{id: id, cost: cost})
		case event.AgentPRCreated:
			prURL, ok := evt.Data["pr_url"].(string)
			if !ok {
				prURL = ""
			}
			prCreated = append(prCreated, fmt.Sprintf("#%s (%s)", prNumberFromURL(prURL), id))
		case event.AgentBranchPushed:
			branch, _ := evt.Data["branch"].(string)
			sha, _ := evt.Data["sha"].(string)
			branchPushed = append(branchPushed, runNote{id: id, text: fmt.Sprintf("%s@%s (%s)", branch, shortSHA(sha), id)})
		case event.AgentChangeReady:
			repo, _ := evt.Data["repo"].(string)
			handle, _ := evt.Data["handle"].(string)
			version, _ := evt.Data["version"].(float64)
			changeReady = append(changeReady, fmt.Sprintf("%s %s v%d (%s)", repo, handle, int(version), id))
			readyRuns[id], readied[id] = true, true
		case event.AgentChangeDraft:
			repo, _ := evt.Data["repo"].(string)
			handle, _ := evt.Data["handle"].(string)
			version, _ := evt.Data["version"].(float64)
			changeDraft = append(changeDraft, runNote{id: id, text: fmt.Sprintf("%s %s v%d (%s)", repo, handle, int(version), id)})
			readyRuns[id] = true
		case event.AgentCIPassed:
			prURL, ok := evt.Data["pr_url"].(string)
			if !ok {
				prURL = ""
			}
			ciPassed = append(ciPassed, fmt.Sprintf("#%s (%s)", prNumberFromURL(prURL), id))
		case event.AgentCIFailed:
			prURL, ok := evt.Data["pr_url"].(string)
			if !ok {
				prURL = ""
			}
			attempt, ok := evt.Data["attempt"].(float64)
			if !ok {
				attempt = 0
			}
			ciFailed = append(ciFailed, fmt.Sprintf("#%s attempt %.0f (%s)", prNumberFromURL(prURL), attempt, id))
		case event.AgentNeedsAttention:
			if superseded[evt.RunID] {
				continue
			}
			reason, ok := evt.Data["reason"].(string)
			if !ok {
				reason = "unknown"
			}
			needsAttention = append(needsAttention, fmt.Sprintf("%s — %s", id, reason))
		case event.PRAwaitingApproval:
			prURL, ok := evt.Data["pr_url"].(string)
			if !ok {
				prURL = ""
			}
			prsReady = append(prsReady, fmt.Sprintf("#%s (%s)", prNumberFromURL(prURL), id))
		case event.PRMerged:
			prURL, ok := evt.Data["pr_url"].(string)
			if !ok {
				prURL = ""
			}
			prMerged = append(prMerged, fmt.Sprintf("#%s", prNumberFromURL(prURL)))
		case event.ChangeStale, event.ChangeRejected, event.ChangeFailed:
			changeTurnedBack = append(changeTurnedBack, changeNote(evt)+" "+strings.TrimPrefix(evt.Type, "change:"))
		case event.ChangeApproved:
			changeApproved = append(changeApproved, changeNote(evt))
		case event.ChangeNewVersion:
			note := changeNote(evt)
			if u, _ := evt.Data["uploader"].(string); u != "" {
				note += " by " + u
			}
			changeVersions = append(changeVersions, note)
		case event.ChangeIntegrated:
			changeLanded = append(changeLanded, changeNote(evt))
		case event.ChangeWithdrawn:
			changeWithdrawn = append(changeWithdrawn, changeNote(evt))
		}
	}

	// Print in priority order: attention first, then actionable, then info
	if len(needsAttention) > 0 {
		fmt.Printf("%d agent(s) need attention:\n", len(needsAttention))
		for _, s := range needsAttention {
			fmt.Printf("  %s\n", s)
		}
	}
	if len(changeTurnedBack) > 0 {
		fmt.Printf("%d change(s) stale, rejected or failed: %s\n", len(changeTurnedBack), strings.Join(changeTurnedBack, ", "))
	}
	if len(prsReady) > 0 {
		fmt.Printf("%d PR(s) awaiting approval: %s\n", len(prsReady), strings.Join(prsReady, ", "))
	}
	if len(prCreated) > 0 {
		fmt.Printf("%d PR(s) created: %s\n", len(prCreated), strings.Join(prCreated, ", "))
	}
	if len(changeReady) > 0 {
		fmt.Printf("%d change(s) awaiting approval: %s\n", len(changeReady), strings.Join(changeReady, ", "))
	}
	// A draft that klaus ready marked ready since is listed as ready.
	var drafts []string
	for _, d := range changeDraft {
		if !readied[d.id] {
			drafts = append(drafts, d.text)
		}
	}
	if len(drafts) > 0 {
		fmt.Printf("%d change(s) awaiting review: %s\n", len(drafts), strings.Join(drafts, ", "))
	}
	if len(changeApproved) > 0 {
		fmt.Printf("%d change(s) approved: %s\n", len(changeApproved), strings.Join(changeApproved, ", "))
	}
	if len(changeVersions) > 0 {
		fmt.Printf("%d new change version(s): %s\n", len(changeVersions), strings.Join(changeVersions, ", "))
	}
	// A branch recorded as a change is listed once, as the change.
	var awaiting []string
	for _, b := range branchPushed {
		if !readyRuns[b.id] {
			awaiting = append(awaiting, b.text)
		}
	}
	if len(awaiting) > 0 {
		fmt.Printf("%d branch(es) awaiting integration: %s\n", len(awaiting), strings.Join(awaiting, ", "))
	}
	if len(ciPassed) > 0 {
		fmt.Printf("%d CI passed: %s\n", len(ciPassed), strings.Join(ciPassed, ", "))
	}
	if len(ciFailed) > 0 {
		fmt.Printf("%d CI failed: %s\n", len(ciFailed), strings.Join(ciFailed, ", "))
	}
	if len(prMerged) > 0 {
		fmt.Printf("%d PR(s) merged: %s\n", len(prMerged), strings.Join(prMerged, ", "))
	}
	if len(changeLanded) > 0 {
		fmt.Printf("%d change(s) integrated: %s\n", len(changeLanded), strings.Join(changeLanded, ", "))
	}
	if len(changeWithdrawn) > 0 {
		fmt.Printf("%d change(s) withdrawn: %s\n", len(changeWithdrawn), strings.Join(changeWithdrawn, ", "))
	}
	if len(completed) > 0 {
		var totalCost float64
		for _, c := range completed {
			totalCost += c.cost
		}
		fmt.Printf("%d agent(s) completed since last check ($%.2f total)\n", len(completed), totalCost)
	}
}

// changeNote names the change version a change:* event concerns:
// "<repo> <handle> v<version>".
func changeNote(evt event.Event) string {
	repo, _ := evt.Data["repo"].(string)
	handle, _ := evt.Data["handle"].(string)
	version, _ := evt.Data["version"].(float64)
	return fmt.Sprintf("%s %s v%d", repo, handle, int(version))
}

// runNote is a summary line's entry for one run.
type runNote struct {
	id, text string
}

type completedInfo struct {
	id   string
	cost float64
}

func prNumberFromURL(url string) string {
	n := extractPRNumberFromURL(url)
	if n == "" {
		return "?"
	}
	return n
}

func loadMarker(baseDir string) string {
	data, err := os.ReadFile(filepath.Join(baseDir, markerFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveMarker(baseDir, marker string) {
	if err := os.WriteFile(filepath.Join(baseDir, markerFile), []byte(marker+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save notification marker: %v\n", err)
	}
}

func init() {
	notificationsCmd.Flags().Bool("all", false, "Show all events, not just since last check")
	notificationsCmd.Flags().Bool("json", false, "Output events as JSON")
	rootCmd.AddCommand(notificationsCmd)
}

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/patflynn/klaus/internal/event"
	"github.com/spf13/cobra"
)

const markerFile = "notifications-marker"

var notificationsCmd = &cobra.Command{
	Use:   "notifications",
	Short: "Show recent events and actionable items",
	Long: `Displays events since the last check, grouped into actionable summaries.

By default shows only events since the last time notifications was run.
Use --all to show all events, or --json for machine-readable output.`,
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

		printSummary(events)
		return nil
	},
}

func printSummary(events []event.Event) {
	var (
		prsReady      []string
		needsAttention []string
		completed     []completedInfo
		prCreated     []string
		ciFailed      []string
		ciPassed      []string
		prMerged      []string
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
		}
	}

	// Print in priority order: attention first, then actionable, then info
	if len(needsAttention) > 0 {
		fmt.Printf("%d agent(s) need attention:\n", len(needsAttention))
		for _, s := range needsAttention {
			fmt.Printf("  %s\n", s)
		}
	}
	if len(prsReady) > 0 {
		fmt.Printf("%d PR(s) awaiting approval: %s\n", len(prsReady), strings.Join(prsReady, ", "))
	}
	if len(prCreated) > 0 {
		fmt.Printf("%d PR(s) created: %s\n", len(prCreated), strings.Join(prCreated, ", "))
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
	if len(completed) > 0 {
		var totalCost float64
		for _, c := range completed {
			totalCost += c.cost
		}
		fmt.Printf("%d agent(s) completed since last check ($%.2f total)\n", len(completed), totalCost)
	}
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

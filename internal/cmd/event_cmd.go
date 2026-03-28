package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

var eventCmd = &cobra.Command{
	Use:   "_event",
	Short: "Emit an event to the session event log",
	Long: `Appends an event to the current session's event log (events.jsonl).
This is an internal command called by agents from shell pipelines.`,
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		runID, _ := cmd.Flags().GetString("run-id")
		eventType, _ := cmd.Flags().GetString("type")
		dataStr, _ := cmd.Flags().GetString("data")

		if runID == "" {
			return fmt.Errorf("--run-id is required")
		}
		if eventType == "" {
			return fmt.Errorf("--type is required")
		}

		var data map[string]interface{}
		if dataStr != "" {
			if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
				return fmt.Errorf("parsing --data JSON: %w", err)
			}
		}

		store, err := sessionStore()
		if err != nil {
			return nil // silently ignore outside session
		}
		hds, ok := store.(*run.HomeDirStore)
		if !ok {
			return nil
		}

		log := event.NewLog(hds.BaseDir())
		evt := event.New(runID, eventType, data)
		return log.Emit(evt)
	},
}

// emitEvent is a helper for emitting events from within commands that
// already have access to the session base directory. Errors are silently
// ignored (best effort).
func emitEvent(baseDir, runID, eventType string, data map[string]interface{}) {
	l := event.NewLog(baseDir)
	evt := event.New(runID, eventType, data)
	_ = l.Emit(evt)
}

// emitEventFromSession resolves the current session's base dir and emits an event.
// Returns silently if no session is active.
func emitEventFromSession(runID, eventType string, data map[string]interface{}) {
	store, err := sessionStore()
	if err != nil {
		return
	}
	hds, ok := store.(*run.HomeDirStore)
	if !ok {
		return
	}
	emitEvent(hds.BaseDir(), runID, eventType, data)
}

func init() {
	eventCmd.Flags().String("run-id", "", "Run ID that produced the event")
	eventCmd.Flags().String("type", "", "Event type (e.g. agent:completed)")
	eventCmd.Flags().String("data", "", "JSON payload for the event")
	rootCmd.AddCommand(eventCmd)
}

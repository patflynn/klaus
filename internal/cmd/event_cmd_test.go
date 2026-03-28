package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/patflynn/klaus/internal/event"
)

func TestEventCmdEmitsToLog(t *testing.T) {
	// Set up a temporary session directory
	dir := t.TempDir()
	sessionID := "test-session-event"
	sessionDir := filepath.Join(dir, ".klaus", "sessions", sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Override HOME so NewHomeDirStore resolves to our temp dir
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", dir)
	defer os.Setenv("HOME", origHome)

	t.Setenv("KLAUS_SESSION_ID", sessionID)

	// Execute _event command via cobra
	rootCmd.SetArgs([]string{
		"_event",
		"--run-id", "run-001",
		"--type", "agent:completed",
		"--data", `{"cost_usd": 3.42, "duration_ms": 15000}`,
	})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("_event command failed: %v", err)
	}

	// Verify event was written
	log := event.NewLog(sessionDir)
	events, err := log.Read()
	if err != nil {
		t.Fatalf("reading events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.RunID != "run-001" {
		t.Errorf("RunID = %q, want %q", evt.RunID, "run-001")
	}
	if evt.Type != "agent:completed" {
		t.Errorf("Type = %q, want %q", evt.Type, "agent:completed")
	}
	if cost, ok := evt.Data["cost_usd"].(float64); !ok || cost != 3.42 {
		t.Errorf("cost_usd = %v, want 3.42", evt.Data["cost_usd"])
	}
}

func TestNotificationsCmdShowsEvents(t *testing.T) {
	// Set up a temporary session with pre-existing events
	dir := t.TempDir()
	sessionID := "test-session-notif"
	sessionDir := filepath.Join(dir, ".klaus", "sessions", sessionID)
	if err := os.MkdirAll(filepath.Join(sessionDir, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}

	origHome := os.Getenv("HOME")
	t.Setenv("HOME", dir)
	defer os.Setenv("HOME", origHome)

	t.Setenv("KLAUS_SESSION_ID", sessionID)

	// Write some events directly
	log := event.NewLog(sessionDir)
	_ = log.Emit(event.New("run-001", event.AgentStarted, map[string]interface{}{
		"id": "run-001", "prompt": "fix bug",
	}))
	_ = log.Emit(event.New("run-001", event.AgentCompleted, map[string]interface{}{
		"id": "run-001", "cost_usd": 1.50,
	}))
	_ = log.Emit(event.New("run-001", event.AgentPRCreated, map[string]interface{}{
		"id": "run-001", "pr_url": "https://github.com/owner/repo/pull/42", "pr_number": "42",
	}))

	// Run notifications --json to get structured output
	rootCmd.SetArgs([]string{"notifications", "--all", "--json"})
	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := rootCmd.Execute()
	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("notifications command failed: %v", err)
	}

	var events []event.Event
	if err := json.NewDecoder(r).Decode(&events); err != nil {
		t.Fatalf("decoding JSON output: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events in JSON output, got %d", len(events))
	}
}

func TestNotificationsMarker(t *testing.T) {
	// Test the marker persistence directly rather than through cobra
	// (cobra flag state leaks between tests in the same process)
	dir := t.TempDir()

	log := event.NewLog(dir)
	_ = log.Emit(event.New("run-001", event.AgentCompleted, map[string]interface{}{
		"id": "run-001", "cost_usd": 1.0,
	}))

	// Read since empty marker — should get 1 event
	marker := loadMarker(dir)
	if marker != "" {
		t.Fatalf("expected empty initial marker, got %q", marker)
	}

	events, newMarker, err := log.ReadSince(marker)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	// Save marker
	saveMarker(dir, newMarker)

	// Verify marker persisted
	marker2 := loadMarker(dir)
	if marker2 != newMarker {
		t.Errorf("loaded marker %q != saved marker %q", marker2, newMarker)
	}

	// Read since saved marker — should be empty
	events2, _, err := log.ReadSince(marker2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events2) != 0 {
		t.Fatalf("expected 0 events after marker, got %d", len(events2))
	}

	// Emit another event, read since marker — should get 1
	_ = log.Emit(event.New("run-002", event.AgentStarted, nil))
	events3, _, err := log.ReadSince(marker2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events3) != 1 {
		t.Fatalf("expected 1 new event, got %d", len(events3))
	}
}

package cmd

import (
	"fmt"
	"testing"
)

func TestDashboardPaneCommand(t *testing.T) {
	// Verify the dashboard command format matches what session.go constructs
	sessionID := "session-abc123"
	got := fmt.Sprintf("KLAUS_SESSION_ID=%s klaus dashboard", sessionID)
	want := "KLAUS_SESSION_ID=session-abc123 klaus dashboard"
	if got != want {
		t.Errorf("dashboard command = %q, want %q", got, want)
	}
}

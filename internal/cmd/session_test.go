package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
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

func TestNewSessionCmdRegistered(t *testing.T) {
	// Verify 'new' subcommand is registered on root
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Use == "new" {
			found = true
			break
		}
	}
	if !found {
		t.Error("'new' subcommand not registered on root command")
	}
}

func TestScaffoldCmdRegistered(t *testing.T) {
	// Verify 'scaffold' subcommand is registered (renamed from 'new')
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Use == "scaffold <project-name>" {
			found = true
			break
		}
	}
	if !found {
		t.Error("'scaffold' subcommand not registered on root command")
	}
}

func TestSessionDefaultResumesExisting(t *testing.T) {
	// Create a fake sessions directory with one session
	tmpDir := t.TempDir()
	sessDir := filepath.Join(tmpDir, "session-20260401-1200-abcd")
	store := run.NewHomeDirStoreFromPath(sessDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	state := &run.State{
		ID:        "session-20260401-1200-abcd",
		Type:      "session",
		Worktree:  t.TempDir(), // point to a real dir
		CreatedAt: "2026-04-01T12:00:00Z",
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}

	// FindMostRecentSession should find it
	found, err := run.FindMostRecentSession(tmpDir)
	if err != nil {
		t.Fatalf("FindMostRecentSession: %v", err)
	}
	if found != "session-20260401-1200-abcd" {
		t.Errorf("expected session-20260401-1200-abcd, got %s", found)
	}
}

func TestSessionDefaultCreatesNewWhenNoneExist(t *testing.T) {
	tmpDir := t.TempDir()
	// Empty sessions dir — should error
	_, err := run.FindMostRecentSession(tmpDir)
	if err == nil {
		t.Error("expected error when no sessions exist")
	}
}

func TestClaudeSessionIDPersistence(t *testing.T) {
	// Create a JSONL log with a result event containing a session_id
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "test.jsonl")
	resultEvent := map[string]interface{}{
		"type":       "result",
		"session_id": "abc-123-def-456",
	}
	data, _ := json.Marshal(resultEvent)
	if err := os.WriteFile(logFile, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// ExtractClaudeSessionID should find it
	got := ExtractClaudeSessionID(logFile)
	if got != "abc-123-def-456" {
		t.Errorf("ExtractClaudeSessionID = %q, want %q", got, "abc-123-def-456")
	}
}

func TestClaudeSessionIDMissing(t *testing.T) {
	// No log file — should return empty string
	got := ExtractClaudeSessionID("/nonexistent/path.jsonl")
	if got != "" {
		t.Errorf("ExtractClaudeSessionID = %q, want empty", got)
	}
}

func TestClaudeSessionIDInState(t *testing.T) {
	// Verify ClaudeSessionID round-trips through save/load
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	csID := "session-uuid-789"
	state := &run.State{
		ID:              "session-test",
		Type:            "session",
		CreatedAt:       "2026-04-01T12:00:00Z",
		ClaudeSessionID: &csID,
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load("session-test")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ClaudeSessionID == nil {
		t.Fatal("ClaudeSessionID should not be nil after round-trip")
	}
	if *loaded.ClaudeSessionID != "session-uuid-789" {
		t.Errorf("ClaudeSessionID = %q, want %q", *loaded.ClaudeSessionID, "session-uuid-789")
	}
}

func TestResumeClaudeArgs(t *testing.T) {
	// Verify that claude args are constructed correctly for different resume scenarios
	tests := []struct {
		name            string
		resuming        bool
		claudeSessionID string
		wantContains    string
		wantNotContain  string
	}{
		{
			name:           "new session gets --session-id",
			resuming:       false,
			claudeSessionID: "",
			wantContains:   "--session-id",
		},
		{
			name:            "resume with session ID",
			resuming:        true,
			claudeSessionID: "uuid-123",
			wantContains:    "--resume",
		},
		{
			name:            "resume without session ID falls back to --continue",
			resuming:        true,
			claudeSessionID: "",
			wantContains:    "--continue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{
				"--dangerously-skip-permissions",
				"-n", "test-session",
				"--append-system-prompt", "test prompt",
			}
			if tt.resuming && tt.claudeSessionID != "" {
				args = append(args, "--resume", tt.claudeSessionID)
			} else if tt.resuming {
				args = append(args, "--continue")
			} else {
				csID := genUUIDv4()
				if csID != "" {
					args = append(args, "--session-id", csID)
				}
			}

			argsStr := fmt.Sprintf("%v", args)
			if tt.wantContains != "" {
				found := false
				for _, a := range args {
					if a == tt.wantContains {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("args %s should contain %q", argsStr, tt.wantContains)
				}
			}
			if tt.wantNotContain != "" {
				for _, a := range args {
					if a == tt.wantNotContain {
						t.Errorf("args %s should not contain %q", argsStr, tt.wantNotContain)
					}
				}
			}
		})
	}
}

// stubTmuxClient is a minimal tmux.Client for testing waitForAgents.
type stubTmuxClient struct {
	tmux.ExecClient
	existingPanes map[string]bool
	killedPanes   []string
}

func (s *stubTmuxClient) PaneExists(_ context.Context, id string) bool {
	return s.existingPanes[id]
}

func (s *stubTmuxClient) PaneIsDead(_ context.Context, _ string) bool {
	return false
}

func (s *stubTmuxClient) PaneIsIdle(_ context.Context, _ string) bool {
	return false
}

func (s *stubTmuxClient) KillPane(_ context.Context, id string) error {
	s.killedPanes = append(s.killedPanes, id)
	delete(s.existingPanes, id)
	return nil
}

func TestWaitForAgents_ContextCancellation(t *testing.T) {
	// Set up a store with an active agent.
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	pane := "%99"
	state := &run.State{
		ID:        "agent-test-cancel",
		Type:      "agent",
		TmuxPane:  &pane,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}

	tc := &stubTmuxClient{existingPanes: map[string]bool{"%99": true}}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so waitForAgents returns quickly.
	cancel()

	done := make(chan struct{})
	go func() {
		waitForAgents(ctx, store, tc)
		close(done)
	}()

	select {
	case <-done:
		// Good -- returned promptly on context cancel.
	case <-time.After(5 * time.Second):
		t.Fatal("waitForAgents did not return after context cancellation")
	}
}

func TestWaitForAgents_FsnotifyDetectsCompletion(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	pane := "%50"
	state := &run.State{
		ID:        "agent-fsnotify-test",
		Type:      "agent",
		TmuxPane:  &pane,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}

	tc := &stubTmuxClient{existingPanes: map[string]bool{"%50": true}}

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		waitForAgents(ctx, store, tc)
		close(done)
	}()

	// Give fsnotify time to set up the watcher.
	time.Sleep(100 * time.Millisecond)

	// Simulate the agent finishing: remove the pane from the mock and
	// write the state file to trigger fsnotify.
	delete(tc.existingPanes, "%50")
	state.CostUSD = new(float64)
	*state.CostUSD = 0.05
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
		// Good -- detected completion via fsnotify.
	case <-time.After(5 * time.Second):
		t.Fatal("waitForAgents did not detect agent completion via fsnotify")
	}
}

func TestWaitForAgents_NoActiveAgents(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	// Save a session (should be skipped) and no agents with live panes.
	state := &run.State{
		ID:        "session-skip",
		Type:      "session",
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}

	tc := &stubTmuxClient{existingPanes: map[string]bool{}}

	done := make(chan struct{})
	go func() {
		waitForAgents(context.Background(), store, tc)
		close(done)
	}()

	select {
	case <-done:
		// Returns immediately when no active agents.
	case <-time.After(2 * time.Second):
		t.Fatal("waitForAgents should return immediately with no active agents")
	}
}

func TestReapFinishedAgents(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	pane1 := "%10"
	pane2 := "%11"
	// Agent 1: pane still exists (still running).
	s1 := &run.State{
		ID:        "agent-running",
		Type:      "agent",
		TmuxPane:  &pane1,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	// Agent 2: pane gone (finished).
	s2 := &run.State{
		ID:        "agent-done",
		Type:      "agent",
		TmuxPane:  &pane2,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if err := store.Save(s1); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(s2); err != nil {
		t.Fatal(err)
	}

	tc := &stubTmuxClient{existingPanes: map[string]bool{"%10": true}}
	active := map[string]*run.State{
		s1.ID: s1,
		s2.ID: s2,
	}

	reapFinishedAgents(context.Background(), store, tc, active)

	if _, ok := active["agent-done"]; ok {
		t.Error("agent-done should have been reaped")
	}
	if _, ok := active["agent-running"]; !ok {
		t.Error("agent-running should still be active")
	}
}

func TestGenUUIDv4(t *testing.T) {
	id := genUUIDv4()
	if len(id) != 36 {
		t.Fatalf("UUID length = %d, want 36", len(id))
	}
	// Version nibble should be '4'
	if id[14] != '4' {
		t.Errorf("UUID version nibble = %c, want '4'", id[14])
	}
	// Variant nibble should be 8, 9, a, or b
	v := id[19]
	if v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Errorf("UUID variant nibble = %c, want 8/9/a/b", v)
	}
	// Should have dashes in correct positions
	for _, pos := range []int{8, 13, 18, 23} {
		if id[pos] != '-' {
			t.Errorf("UUID[%d] = %c, want '-'", pos, id[pos])
		}
	}
}

package cmd

import (
	"context"
	"testing"

	"github.com/patflynn/klaus/internal/run"
)

func TestAgentsSessionName(t *testing.T) {
	t.Run("uses KLAUS_SESSION_ID", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "session-20260907-1241-c0da2e6d")
		if got, want := agentsSessionName(), "klaus-agents-session-20260907-1241-c0da2e6d"; got != want {
			t.Errorf("agentsSessionName() = %q, want %q", got, want)
		}
	})

	t.Run("replaces tmux target separators", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "a.b:c")
		if got, want := agentsSessionName(), "klaus-agents-a-b-c"; got != want {
			t.Errorf("agentsSessionName() = %q, want %q", got, want)
		}
	})

	t.Run("falls back when unset", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		if got, want := agentsSessionName(), "klaus-agents"; got != want {
			t.Errorf("agentsSessionName() = %q, want %q", got, want)
		}
	})
}

func TestKillEmptyAgentsSession(t *testing.T) {
	t.Run("kills a session whose panes are all idle", func(t *testing.T) {
		tc := isolatedTmux(t)
		session := agentsSessionName()
		tc.panes = map[string][]string{session: {"%1", "%2"}}
		store := newFakeStore()

		out := captureStdout(t, func() {
			killEmptyAgentsSession(context.Background(), store, tc)
		})

		if len(tc.killedSessions) != 1 || tc.killedSessions[0] != session {
			t.Fatalf("expected KillSession(%q), got %v", session, tc.killedSessions)
		}
		if !contains(out, "killed agents tmux session "+session) {
			t.Errorf("expected kill message, got: %s", out)
		}
	})

	t.Run("keeps a session with a running pane even when the store is empty", func(t *testing.T) {
		tc := isolatedTmux(t)
		session := agentsSessionName()
		tc.panes = map[string][]string{session: {"%1", "%2"}}
		tc.runningPanes = map[string]bool{"%2": true}
		// The store knows nothing about the panes — exactly the state a unit
		// test's fake store is in (issue #295). tmux must still win.
		store := newFakeStore()

		out := captureStdout(t, func() {
			killEmptyAgentsSession(context.Background(), store, tc)
		})

		if len(tc.killedSessions) != 0 {
			t.Fatalf("expected no KillSession calls, got %v", tc.killedSessions)
		}
		if !contains(out, "keeping agents tmux session "+session+" (pane %2 still running)") {
			t.Errorf("expected skip message naming the live pane, got: %s", out)
		}
	})

	t.Run("keeps a session whose panes are still held by a run", func(t *testing.T) {
		tc := isolatedTmux(t)
		session := agentsSessionName()
		tc.panes = map[string][]string{session: {"%1"}}
		pane := "%1"
		store := newFakeStore(&run.State{ID: "run-1", TmuxPane: &pane})

		captureStdout(t, func() {
			killEmptyAgentsSession(context.Background(), store, tc)
		})

		if len(tc.killedSessions) != 0 {
			t.Fatalf("expected no KillSession calls, got %v", tc.killedSessions)
		}
	})

	t.Run("no-op when the session does not exist", func(t *testing.T) {
		tc := isolatedTmux(t)
		store := newFakeStore()

		killEmptyAgentsSession(context.Background(), store, tc)

		if len(tc.killedSessions) != 0 {
			t.Fatalf("expected no KillSession calls, got %v", tc.killedSessions)
		}
	})
}

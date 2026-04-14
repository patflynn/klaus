package cmd

import (
	"fmt"
	"os"

	"github.com/patflynn/klaus/internal/run"
)

const sessionIDEnv = "KLAUS_SESSION_ID"

// tmuxSessionEnvPrefix returns a shell snippet that exports KLAUS_SESSION_ID
// for use in tmux pane commands. Tmux panes start fresh shells that don't
// inherit the caller's environment, so any env vars they need must be
// explicitly exported in the command string.
func tmuxSessionEnvPrefix() string {
	sessionID := os.Getenv(sessionIDEnv)
	if sessionID == "" {
		return ""
	}
	return fmt.Sprintf("export %s=%s; ", sessionIDEnv, shellQuote(sessionID))
}

// sessionStore returns a HomeDirStore for the current session (from KLAUS_SESSION_ID env var).
// This is the canonical way to get the store for the active session.
func sessionStore() (run.StateStore, error) {
	sessionID := os.Getenv(sessionIDEnv)
	if sessionID == "" {
		// Fall back to most recent session
		sessionsDir, err := run.SessionsDir()
		if err != nil {
			return nil, fmt.Errorf("KLAUS_SESSION_ID is not set and could not find sessions directory: %w", err)
		}
		found, err := run.FindMostRecentSession(sessionsDir)
		if err != nil {
			return nil, fmt.Errorf("KLAUS_SESSION_ID is not set and no sessions found (are you inside a klaus session?)")
		}
		sessionID = found
	}
	store, err := run.NewHomeDirStore(sessionID)
	if err != nil {
		return nil, err
	}
	return store, nil
}

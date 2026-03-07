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
func sessionStore() (run.StateStore, error) {
	sessionID := os.Getenv(sessionIDEnv)
	if sessionID == "" {
		return nil, fmt.Errorf("KLAUS_SESSION_ID is not set (are you inside a klaus session?)")
	}
	store, err := run.NewHomeDirStore(sessionID)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// sessionStoreOrAll returns a store for the current session if KLAUS_SESSION_ID is set,
// otherwise returns nil (callers should use ListAllSessions to scan all sessions).
func sessionStoreOrAll() (run.StateStore, error) {
	sessionID := os.Getenv(sessionIDEnv)
	if sessionID == "" {
		return nil, nil
	}
	store, err := run.NewHomeDirStore(sessionID)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// listStatesFromEnvOrAll lists states from the current session if KLAUS_SESSION_ID
// is set, otherwise lists states from all sessions under ~/.klaus/sessions/.
func listStatesFromEnvOrAll() ([]*run.State, run.StateStore, error) {
	store, err := sessionStoreOrAll()
	if err != nil {
		return nil, nil, err
	}
	if store != nil {
		states, err := store.List()
		return states, store, err
	}
	// No session env set — scan all sessions
	states, err := run.ListAllSessions()
	return states, nil, err
}

// loadStateFromEnvOrAll loads a specific run state. If KLAUS_SESSION_ID is set,
// looks only in that session. Otherwise scans all sessions.
func loadStateFromEnvOrAll(id string) (*run.State, run.StateStore, error) {
	store, err := sessionStoreOrAll()
	if err != nil {
		return nil, nil, err
	}
	if store != nil {
		state, err := store.Load(id)
		if err != nil {
			return nil, nil, fmt.Errorf("no run found with id: %s", id)
		}
		return state, store, nil
	}
	// Scan all sessions
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("resolving home dir: %w", err)
	}
	return run.FindStateInSessions(home, id)
}

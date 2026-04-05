package run

import (
	"fmt"
	"os"
	"path/filepath"
)

// HomeDirStore implements StateStore using ~/.klaus/sessions/{session-id}/.
type HomeDirStore struct {
	baseDir string // e.g. ~/.klaus/sessions/{session-id}
}

// NewHomeDirStore creates a new HomeDirStore for the given session ID.
// It resolves ~/.klaus via os.UserHomeDir().
func NewHomeDirStore(sessionID string) (*HomeDirStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home dir: %w", err)
	}
	baseDir := filepath.Join(home, ".klaus", "sessions", sessionID)
	return &HomeDirStore{baseDir: baseDir}, nil
}

// NewHomeDirStoreFromPath creates a HomeDirStore with an explicit base directory.
// Useful for testing.
func NewHomeDirStoreFromPath(baseDir string) *HomeDirStore {
	return &HomeDirStore{baseDir: baseDir}
}

func (s *HomeDirStore) StateDir() string {
	return filepath.Join(s.baseDir, "runs")
}

func (s *HomeDirStore) LogDir() string {
	return filepath.Join(s.baseDir, "logs")
}

func (s *HomeDirStore) BaseDir() string {
	return s.baseDir
}

func (s *HomeDirStore) EnsureDirs() error {
	if err := os.MkdirAll(s.StateDir(), 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	if err := os.MkdirAll(s.LogDir(), 0o755); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}
	return nil
}

func (s *HomeDirStore) Save(st *State) error {
	return saveState(s.StateDir(), st)
}

func (s *HomeDirStore) Load(id string) (*State, error) {
	return loadState(s.StateDir(), id)
}

func (s *HomeDirStore) List() ([]*State, error) {
	return listStates(s.StateDir())
}

func (s *HomeDirStore) Delete(id string) error {
	return deleteState(s.StateDir(), id)
}

// ListAllSessions scans ~/.klaus/sessions/ and returns a combined list of
// all run states across all session directories.
func ListAllSessions() ([]*State, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home dir: %w", err)
	}
	return listAllSessionsIn(filepath.Join(home, ".klaus", "sessions"))
}

func listAllSessionsIn(sessionsDir string) ([]*State, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading sessions dir: %w", err)
	}

	var all []*State
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		store := NewHomeDirStoreFromPath(filepath.Join(sessionsDir, e.Name()))
		states, err := store.List()
		if err != nil {
			continue
		}
		all = append(all, states...)
	}
	return all, nil
}

// FindMostRecentSession returns the ID of the most recent session directory
// under the given sessions base dir. Session IDs embed timestamps
// (session-YYYYMMDD-HHMM-XXXX), so lexicographic sort gives chronological order.
func FindMostRecentSession(sessionsDir string) (string, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no sessions found")
		}
		return "", fmt.Errorf("reading sessions dir: %w", err)
	}

	var latest string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) > len("session-") && name[:8] == "session-" {
			latest = name
		}
	}
	if latest == "" {
		return "", fmt.Errorf("no sessions found")
	}
	return latest, nil
}

// FindStateInSessions searches across all session directories for a run with the given ID.
// Returns the state and its store if found.
func FindStateInSessions(homeDir string, id string) (*State, StateStore, error) {
	sessionsDir := filepath.Join(homeDir, ".klaus", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("no run found with id: %s", id)
		}
		return nil, nil, fmt.Errorf("reading sessions dir: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		store := NewHomeDirStoreFromPath(filepath.Join(sessionsDir, e.Name()))
		st, err := store.Load(id)
		if err == nil {
			return st, store, nil
		}
	}
	return nil, nil, fmt.Errorf("no run found with id: %s", id)
}

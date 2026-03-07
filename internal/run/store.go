package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Shared helpers used by both GitDirStore and HomeDirStore.

func saveState(dir string, st *State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	path := filepath.Join(dir, st.ID+".json")
	return os.WriteFile(path, data, 0o644)
}

func loadState(dir string, id string) (*State, error) {
	path := filepath.Join(dir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading state file: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing state file: %w", err)
	}
	return &st, nil
}

func listStates(dir string) ([]*State, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading state dir: %w", err)
	}

	var states []*State
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		st, err := loadState(dir, id)
		if err != nil {
			continue // skip corrupt files
		}
		states = append(states, st)
	}

	sort.Slice(states, func(i, j int) bool {
		return states[i].CreatedAt > states[j].CreatedAt
	})

	return states, nil
}

func deleteState(dir string, id string) error {
	path := filepath.Join(dir, id+".json")
	return os.Remove(path)
}

// StateStore defines the interface for persisting run state.
type StateStore interface {
	Save(s *State) error
	Load(id string) (*State, error)
	List() ([]*State, error)
	Delete(id string) error
	LogDir() string
	StateDir() string
	EnsureDirs() error
}

// GitDirStore implements StateStore using the .git/klaus/ directory structure.
type GitDirStore struct {
	gitCommonDir string
}

// NewGitDirStore creates a new GitDirStore rooted at the given git common directory.
func NewGitDirStore(gitCommonDir string) *GitDirStore {
	return &GitDirStore{gitCommonDir: gitCommonDir}
}

func (s *GitDirStore) StateDir() string {
	return filepath.Join(s.gitCommonDir, "klaus", "runs")
}

func (s *GitDirStore) LogDir() string {
	return filepath.Join(s.gitCommonDir, "klaus", "logs")
}

func (s *GitDirStore) EnsureDirs() error {
	if err := os.MkdirAll(s.StateDir(), 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	if err := os.MkdirAll(s.LogDir(), 0o755); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}
	return nil
}

func (s *GitDirStore) Save(st *State) error {
	return saveState(s.StateDir(), st)
}

func (s *GitDirStore) Load(id string) (*State, error) {
	return loadState(s.StateDir(), id)
}

func (s *GitDirStore) List() ([]*State, error) {
	return listStates(s.StateDir())
}

func (s *GitDirStore) Delete(id string) error {
	return deleteState(s.StateDir(), id)
}

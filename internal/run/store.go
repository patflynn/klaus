package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
	dir := s.StateDir()
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

func (s *GitDirStore) Load(id string) (*State, error) {
	path := filepath.Join(s.StateDir(), id+".json")
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

func (s *GitDirStore) List() ([]*State, error) {
	dir := s.StateDir()
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
		st, err := s.Load(id)
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

func (s *GitDirStore) Delete(id string) error {
	path := filepath.Join(s.StateDir(), id+".json")
	return os.Remove(path)
}

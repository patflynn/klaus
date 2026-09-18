package consult

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/patflynn/klaus/internal/backend"
)

type Thread struct {
	Backend          backend.Kind `json:"backend"`
	Model            string       `json:"model"`
	Effort           string       `json:"effort,omitempty"`
	BackendSessionID string       `json:"backend_session_id"`
	Dir              string       `json:"dir"`
	Role             string       `json:"role"`
	CreatedAt        time.Time    `json:"created_at"`
	LastUsed         time.Time    `json:"last_used"`
	Turns            int          `json:"turns"`
}

type Store struct{ Dir string }

func NewStore(sessionDir string) Store { return Store{Dir: filepath.Join(sessionDir, "consults")} }

var threadName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func (s Store) path(name, ext string) (string, error) {
	if !threadName.MatchString(name) || len(name) > 120 {
		return "", fmt.Errorf("invalid thread name %q: use up to 120 letters, digits, dots, underscores or hyphens, starting with a letter or digit", name)
	}
	return filepath.Join(s.Dir, name+ext), nil
}

func (s Store) Lock(name string) (func(), error) {
	path, err := s.path(name, ".lock")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("locking thread %q (another consult may be running): %w", name, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("thread %q is already in use: %w", name, err)
	}
	return func() { _ = f.Close() }, nil
}

func (s Store) Load(name string) (*Thread, error) {
	path, err := s.path(name, ".json")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Thread
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s Store) Save(name string, t *Thread) error {
	path, err := s.path(name, ".json")
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".thread-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (s Store) Append(name, prompt, answer string) error {
	path, err := s.path(name, ".log")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "## %s\n\n### Prompt\n%s\n\n### Answer\n%s\n\n", time.Now().UTC().Format(time.RFC3339), prompt, answer)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s Store) List() (map[string]*Thread, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return map[string]*Thread{}, nil
	}
	if err != nil {
		return nil, err
	}
	threads := make(map[string]*Thread)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		t, err := s.Load(name)
		if err != nil {
			return nil, fmt.Errorf("reading thread %s: %w", name, err)
		}
		threads[name] = t
	}
	return threads, nil
}

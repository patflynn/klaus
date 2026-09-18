package consult

import (
	"bytes"
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
	return writeAtomic(path, append(data, '\n'))
}

type Turn struct {
	Number           int          `json:"turn"`
	Status           string       `json:"status"`
	StartedAt        time.Time    `json:"started_at"`
	FinishedAt       *time.Time   `json:"finished_at,omitempty"`
	Backend          backend.Kind `json:"backend"`
	Model            string       `json:"model"`
	ResumeID         string       `json:"resume_id"`
	RepoRevision     string       `json:"repo_revision"`
	SessionIDSource  string       `json:"session_id_source"`
	BackendSessionID string       `json:"backend_session_id,omitempty"`
	Prompt           string       `json:"prompt"`
	Answer           string       `json:"answer,omitempty"`
	RawOutput        string       `json:"raw_output,omitempty"`
	prior, pending   []byte
}

// BeginTurn persists intent before execution; callers hold the thread lock.
func (s Store) BeginTurn(name string, t *Thread, prompt, revision string) (*Turn, error) {
	path, err := s.path(name, ".log")
	if err != nil {
		return nil, err
	}
	prior, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(prior) > 0 && prior[len(prior)-1] != '\n' {
		prior = append(prior, '\n')
	}
	number := t.Turns + 1
	for _, line := range bytes.Split(prior, []byte("\n")) {
		var previous Turn
		if json.Unmarshal(line, &previous) == nil && previous.Number >= number {
			number = previous.Number + 1
		}
	}
	turn := &Turn{Number: number, Status: "pending", StartedAt: time.Now().UTC(), Backend: t.Backend, Model: t.Model, ResumeID: t.BackendSessionID, RepoRevision: revision, Prompt: prompt, prior: prior}
	data, err := json.Marshal(turn)
	if err != nil {
		return nil, err
	}
	turn.pending = append(data, '\n')
	if err := writeAtomic(path, append(bytes.Clone(prior), turn.pending...)); err != nil {
		return nil, err
	}
	return turn, nil
}

func (s Store) FinishTurn(name string, turn *Turn, result Result, turnErr error) error {
	path, err := s.path(name, ".log")
	if err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, append(bytes.Clone(turn.prior), turn.pending...)) {
		return fmt.Errorf("transcript changed while turn %d was running", turn.Number)
	}
	turn.Status = "ok"
	if turnErr != nil {
		turn.Status = "error: " + turnErr.Error()
	}
	now := time.Now().UTC()
	turn.FinishedAt = &now
	turn.Answer = result.Answer
	turn.SessionIDSource = result.SessionIDSource
	turn.BackendSessionID = result.SessionID
	data, err := json.Marshal(turn)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(bytes.Clone(turn.prior), append(data, '\n')...))
}

func (s Store) SaveRaw(name string, number int, result Result) (string, error) {
	path, err := s.path(name, fmt.Sprintf(".%d.raw", number))
	if err != nil {
		return "", err
	}
	raw := "### stdout\n" + result.Answer + "\n### stderr\n" + result.Stderr
	if result.DiagnosticLog != "" {
		raw += "\n### diagnostic-log\n" + result.DiagnosticLog
	}
	err = writeAtomic(path, []byte(raw))
	return path, err
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".consult-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
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

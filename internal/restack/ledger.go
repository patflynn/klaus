package restack

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// LedgerFile is the ledger's file name in a session directory.
const LedgerFile = "restack-dispatches.json"

// Entry is what klaus did about one change's restack conflicts.
type Entry struct {
	Repo     string `json:"repo"`
	ChangeID string `json:"change_id"`
	Handle   string `json:"handle,omitempty"`
	// Attempts counts the resolution runs dispatched since the change last
	// restacked cleanly.
	Attempts int `json:"attempts"`
	// Last is the latest conflict klaus acted on.
	Last *Action `json:"last,omitempty"`
	// Stopped is when klaus stopped dispatching for the change, its
	// attempts spent (RFC3339); "" while it hasn't.
	Stopped string `json:"stopped,omitempty"`
}

// Action is what klaus did about one conflict: the report and the inputs it
// acted on, and the run it launched or why it launched none.
type Action struct {
	At        time.Time `json:"at"`
	Report    string    `json:"report"`
	Head      string    `json:"head"`
	TargetTip string    `json:"target_tip"`
	RunID     string    `json:"run_id,omitempty"`
	Refused   string    `json:"refused,omitempty"`
}

// Ledger is the session's record of resolution runs, by change. It outlives
// the dashboard, so cooldowns and attempt counts survive a restart, and a
// pass run by hand sees what the dashboard's did.
type Ledger struct {
	path    string // "" keeps it in memory
	entries map[string]*Entry
}

// NewLedger returns an empty ledger kept in memory.
func NewLedger() *Ledger { return &Ledger{entries: make(map[string]*Entry)} }

// LoadLedger reads the ledger in a session directory; a missing file is an
// empty ledger.
func LoadLedger(dir string) (*Ledger, error) {
	l := &Ledger{path: filepath.Join(dir, LedgerFile), entries: make(map[string]*Entry)}
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(data, &l.entries); err != nil {
		return l, fmt.Errorf("parsing %s: %w", l.path, err)
	}
	return l, nil
}

// Lock takes an exclusive lock on a session directory's ledger, held by
// one pass at a time across processes, so the dashboard and a pass run by
// hand never both dispatch for a conflict.
func Lock(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, LedgerFile+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, nil
}

// Entry returns the entry of a change, or nil for none.
func (l *Ledger) Entry(k Key) *Entry { return l.entries[k.String()] }

// put stores an entry.
func (l *Ledger) put(e *Entry) {
	l.entries[Key{Repo: e.Repo, ChangeID: e.ChangeID}.String()] = e
}

// save writes the ledger through a temporary file, so a crash mid-write
// leaves the previous one in place.
func (l *Ledger) save() error {
	if l.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(l.entries, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(l.path), ".restack-dispatches-*")
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
	return os.Rename(f.Name(), l.path)
}

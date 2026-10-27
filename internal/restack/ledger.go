package restack

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// restacked cleanly, including a launch that may have started a run.
	Attempts int `json:"attempts"`
	// Last is the latest conflict klaus acted on.
	Last *Action `json:"last,omitempty"`
	// Consumed is every set of inputs klaus acted on, whatever came of it:
	// klaus acts once on each, so a conflict that comes back to inputs it
	// acted on before isn't tried again.
	Consumed []Inputs `json:"consumed,omitempty"`
	// Clean is the report commit of the last clean restack that gave the
	// change its attempts back, so one report resets them once.
	Clean string `json:"clean,omitempty"`
	// Stopped is when klaus stopped dispatching for the change, its
	// attempts spent (RFC3339); "" while it hasn't.
	Stopped string `json:"stopped,omitempty"`
}

// Inputs are what a resolution run is dispatched on: the change's head and
// the target's tip. The repo and change are the entry's.
type Inputs struct {
	Head      string `json:"head"`
	TargetTip string `json:"target_tip"`
}

// consumed reports whether klaus acted on the inputs before. A ledger
// written before Consumed was kept names only its last action's.
func (e *Entry) consumed(in Inputs) bool {
	if e == nil {
		return false
	}
	for _, c := range e.Consumed {
		if c == in {
			return true
		}
	}
	return e.Last != nil && e.Last.Head == in.Head && e.Last.TargetTip == in.TargetTip
}

// consume records that klaus acted on the inputs.
func (e *Entry) consume(in Inputs) {
	for _, c := range e.Consumed {
		if c == in {
			return
		}
	}
	e.Consumed = append(e.Consumed, in)
}

// Action is what klaus did about one conflict: the report and the inputs it
// acted on, and the run it launched or why it launched none.
type Action struct {
	At        time.Time `json:"at"`
	Report    string    `json:"report"`
	Version   int       `json:"version,omitempty"`
	Head      string    `json:"head"`
	TargetTip string    `json:"target_tip"`
	// RunID is the run klaus launched, or may have: klaus names the run
	// before it launches it. "" once it knows no worker started.
	RunID string `json:"run_id,omitempty"`
	// Pending is set from before klaus launches RunID until it knows
	// whether the run started. A pass that finds it set finds the launch
	// in flight, or the pass that started it gone before it could tell
	// (reconcile).
	Pending bool `json:"pending,omitempty"`
	// Refused says why no run, or no run klaus can vouch for, came of it.
	Refused string `json:"refused,omitempty"`
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

// sorted returns the entries in key order.
func (l *Ledger) sorted() []*Entry {
	keys := make([]string, 0, len(l.entries))
	for k := range l.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*Entry, len(keys))
	for i, k := range keys {
		out[i] = l.entries[k]
	}
	return out
}

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

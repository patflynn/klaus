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
	// Actions are what klaus did on each set of inputs it acted on,
	// whatever came of it, oldest first: klaus acts once on each, so a
	// conflict that comes back to inputs it acted on before isn't tried
	// again, and shows what came of them then. It keeps the latest
	// MaxActions.
	Actions []*Action `json:"actions,omitempty"`
	// Clean is the report commit of the last clean restack that gave the
	// change its attempts back, so one report resets them once.
	Clean string `json:"clean,omitempty"`
	// Stopped is when klaus stopped dispatching for the change, its
	// attempts spent (RFC3339); "" while it hasn't.
	Stopped string `json:"stopped,omitempty"`

	// A ledger written before Actions was kept holds its latest action in
	// "last", and every set of inputs acted on in "consumed"; LoadLedger
	// moves them into Actions.
	OldLast     *Action  `json:"last,omitempty"`
	OldConsumed []Inputs `json:"consumed,omitempty"`
}

// MaxActions is how many actions the ledger keeps for one change, its
// latest: the inputs of older ones may be acted on again. With an entry
// only for each change klaus acted on that hasn't landed or been
// withdrawn, that bounds the ledger.
const MaxActions = 16

// Inputs are what a resolution run is dispatched on: the change's head and
// the target's tip. The repo and change are the entry's.
type Inputs struct {
	Head      string `json:"head"`
	TargetTip string `json:"target_tip"`
}

// Last is the latest action, or nil for none.
func (e *Entry) Last() *Action {
	if e == nil || len(e.Actions) == 0 {
		return nil
	}
	return e.Actions[len(e.Actions)-1]
}

// ActionOn is the action on the inputs, or nil when klaus didn't act on
// them, or no longer remembers it did.
func (e *Entry) ActionOn(in Inputs) *Action {
	if e == nil {
		return nil
	}
	for _, a := range e.Actions {
		if a.inputs() == in {
			return a
		}
	}
	return nil
}

// Pending is the action whose launch klaus can't yet settle, or nil.
func (e *Entry) Pending() *Action {
	if e == nil {
		return nil
	}
	for _, a := range e.Actions {
		if a.Pending {
			return a
		}
	}
	return nil
}

// record adds an action on inputs klaus hasn't acted on, dropping the
// oldest settled ones past MaxActions.
func (e *Entry) record(a *Action) {
	e.Actions = append(e.Actions, a)
	for i := 0; len(e.Actions) > MaxActions && i < len(e.Actions); {
		if e.Actions[i].Pending {
			i++
			continue
		}
		e.Actions = append(e.Actions[:i:i], e.Actions[i+1:]...)
	}
}

// migrate moves what a ledger written before Actions holds into Actions:
// each set of inputs it consumed, oldest first, and its last action, with
// what came of it. Of the others it knows only that klaus acted on them.
func (e *Entry) migrate() {
	if e.OldLast == nil && len(e.OldConsumed) == 0 {
		return
	}
	var old []*Action
	for _, in := range e.OldConsumed {
		if e.ActionOn(in) == nil && (e.OldLast == nil || e.OldLast.inputs() != in) {
			old = append(old, &Action{Head: in.Head, TargetTip: in.TargetTip, Refused: "klaus acted on these inputs before it kept what came of it"})
		}
	}
	if e.OldLast != nil && e.ActionOn(e.OldLast.inputs()) == nil {
		old = append(old, e.OldLast)
	}
	actions := e.Actions
	e.Actions, e.OldLast, e.OldConsumed = nil, nil, nil
	for _, a := range append(old, actions...) {
		e.record(a)
	}
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
	// whether the run started: its run is recorded, or klaus proved no
	// worker of it runs (Deps.WorkerGone). A pass that finds it set finds
	// the launch in flight, one that failed leaving klaus unsure, or the
	// pass that started it gone before it could tell (reconcile).
	Pending bool `json:"pending,omitempty"`
	// Refused says why no run, or no run klaus can vouch for, came of it;
	// on a pending launch, why klaus can't yet settle it.
	Refused string `json:"refused,omitempty"`
}

func (a *Action) inputs() Inputs { return Inputs{Head: a.Head, TargetTip: a.TargetTip} }

// Ledger is the session's record of resolution runs, by change. It outlives
// the dashboard, so cooldowns and attempt counts survive a restart, and a
// pass run by hand sees what the dashboard's did. It is bounded: a pass
// drops a change's entry once the change lands or is withdrawn (prune),
// and an entry keeps MaxActions actions.
type Ledger struct {
	path    string // "" keeps it in memory
	entries map[string]*Entry
}

// NewLedger returns an empty ledger kept in memory.
func NewLedger() *Ledger { return &Ledger{entries: make(map[string]*Entry)} }

// LoadLedger reads the ledger in a session directory, moving what an older
// klaus wrote into the entries' Actions; a missing file is an empty
// ledger.
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
	for _, e := range l.entries {
		e.migrate()
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

// drop removes a change's entry.
func (l *Ledger) drop(k Key) { delete(l.entries, k.String()) }

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

// actionOnReport is the latest action on a report, at a head, or nil.
func (e *Entry) actionOnReport(report, head string) *Action {
	for i := len(e.Actions) - 1; i >= 0; i-- {
		if a := e.Actions[i]; a.Report == report && a.Head == head {
			return a
		}
	}
	return nil
}

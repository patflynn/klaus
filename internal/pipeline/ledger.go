package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LedgerFile is the ledger's file name in a session directory.
const LedgerFile = "pr-transitions.json"

// Ledger is the durable record of what the pipeline has seen and reported for
// each PR, keyed by PR URL. It is what makes pr:approved and pr:merged fire
// once per PR: the controller's per-PR state is rebuilt from GitHub on every
// restart and dropped once a PR is merged or closed, so it can't remember
// that a transition was already reported.
type Ledger struct {
	mu   sync.Mutex
	path string // "" keeps the ledger in memory only
	prs  map[string]*ledgerEntry
}

// ledgerEntry holds, for one PR, when each mark was first recorded (RFC3339).
type ledgerEntry struct {
	SeenOpen string `json:"seen_open,omitempty"` // GitHub first reported it open
	Approved string `json:"approved,omitempty"`  // pr:approved was emitted
	Merged   string `json:"merged,omitempty"`    // GitHub first reported it merged
}

type ledgerMark int

const (
	markSeenOpen ledgerMark = iota
	markApproved
	markMerged
)

func (e *ledgerEntry) field(m ledgerMark) *string {
	switch m {
	case markSeenOpen:
		return &e.SeenOpen
	case markApproved:
		return &e.Approved
	default:
		return &e.Merged
	}
}

// NewLedger returns a ledger kept in memory only, which dedupes within one
// process but forgets on restart.
func NewLedger() *Ledger {
	return &Ledger{prs: make(map[string]*ledgerEntry)}
}

// LoadLedger reads the ledger at path; a missing file is an empty ledger. It
// always returns a usable ledger: if the file can't be read the ledger starts
// empty, the error says why, and the next record rewrites the file.
func LoadLedger(path string) (*Ledger, error) {
	l := &Ledger{path: path, prs: make(map[string]*ledgerEntry)}
	return l, l.mergeFromDisk()
}

// ledgerKey is a PR's key in the ledger: its URL, or its number when the URL
// isn't known.
func ledgerKey(prNumber, prURL string) string {
	if prURL != "" {
		return prURL
	}
	return "#" + prNumber
}

// has reports whether the mark is recorded for the PR.
func (l *Ledger) has(key string, m ledgerMark) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.prs[key]
	return e != nil && *e.field(m) != ""
}

// record sets the mark for the PR and reports whether it is new. A mark
// another process recorded since the ledger was loaded counts as already
// set, so two dashboards on one session don't both report a transition. An
// error means the mark is held in memory but couldn't be saved.
func (l *Ledger) record(key string, m ledgerMark) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.prs[key]; e != nil && *e.field(m) != "" {
		return false, nil
	}
	loadErr := l.mergeFromDisk()
	e := l.prs[key]
	if e == nil {
		e = &ledgerEntry{}
		l.prs[key] = e
	}
	if *e.field(m) != "" {
		return false, nil
	}
	*e.field(m) = time.Now().UTC().Format(time.RFC3339)
	return true, errors.Join(loadErr, l.save())
}

// mergeFromDisk adds the marks in the ledger file to the ones in memory.
// The caller holds l.mu, except in LoadLedger.
func (l *Ledger) mergeFromDisk() error {
	if l.path == "" {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var disk map[string]*ledgerEntry
	if err := json.Unmarshal(data, &disk); err != nil {
		return fmt.Errorf("parsing %s: %w", l.path, err)
	}
	for key, d := range disk {
		if d == nil {
			continue
		}
		e := l.prs[key]
		if e == nil {
			e = &ledgerEntry{}
			l.prs[key] = e
		}
		for _, m := range []ledgerMark{markSeenOpen, markApproved, markMerged} {
			if *e.field(m) == "" {
				*e.field(m) = *d.field(m)
			}
		}
	}
	return nil
}

// save writes the ledger through a temporary file in the same directory, so
// a crash mid-write leaves the previous ledger in place.
func (l *Ledger) save() error {
	if l.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(l.prs, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(l.path)
	f, err := os.CreateTemp(dir, ".pr-transitions-*")
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

package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/valley"
)

// ChangeLedgerFile is the change ledger's file name in a session directory.
const ChangeLedgerFile = "change-transitions.json"

// ChangeLedger is the durable record of what klaus has seen of each valley
// change, keyed by the valley's name for its repo and its change ID. It is
// for a valley change what Ledger is for a PR: it makes each change:* event
// fire once, across dashboard restarts and between dashboards on one
// session, since the valley's listing says only where a change stands now.
type ChangeLedger struct {
	mu      sync.Mutex
	path    string
	changes map[string]*changeEntry
}

// changeEntry holds, for one change, each mark the ledger keeps, with when
// it was first seen.
type changeEntry struct {
	Version   *ChangeMark `json:"version,omitempty"`   // the highest version seen, and its head
	Approved  *ChangeMark `json:"approved,omitempty"`  // the version whose integration request was first seen
	Landed    *ChangeMark `json:"landed,omitempty"`    // the version that landed, and its landed commit
	Outcome   *ChangeMark `json:"outcome,omitempty"`   // the last outcome: stale, rejected, failed or uncertain
	Withdrawn *ChangeMark `json:"withdrawn,omitempty"` // the version active when the change was withdrawn
}

// ChangeMark is a mark the ledger keeps for a change.
type ChangeMark struct {
	Version  int    `json:"version"`
	Head     string `json:"head,omitempty"`
	Commit   string `json:"commit,omitempty"`   // Landed's landed commit
	Decision string `json:"decision,omitempty"` // Outcome's
	At       string `json:"at"`                 // when it was first seen (RFC3339, with nanoseconds)
}

// ChangeTransition is a mark a check found that the ledger didn't hold, and
// the event that reports it.
type ChangeTransition struct {
	Type    string         // an event.Change* type
	Version valley.Version // the version it concerns
	// Commit is the commit the version landed as, for
	// event.ChangeIntegrated: its landed_commit, else its head.
	Commit string
}

// LoadChangeLedger reads the change ledger at path; a missing file is an
// empty ledger. It always returns a usable ledger: if the file can't be read
// the ledger starts empty, the error says why, and the next record rewrites
// the file.
func LoadChangeLedger(path string) (*ChangeLedger, error) {
	l := &ChangeLedger{path: path, changes: make(map[string]*changeEntry)}
	return l, l.mergeFromDisk()
}

// changeLedgerKey is a change's key in the ledger.
func changeLedgerKey(repo, changeID string) string {
	return repo + "/" + changeID
}

// outcomeEvents maps an integrator's decision to the event that reports it.
// Uncertain is recorded, and reported by none.
var outcomeEvents = map[string]string{
	"stale":    event.ChangeStale,
	"rejected": event.ChangeRejected,
	"failed":   event.ChangeFailed,
}

// Observe records where the valley's listing says change ch, in the repo the
// valley calls repo, stands, and returns the transitions the ledger didn't
// hold, in order:
//
//   - change:new-version for each version above the highest the ledger
//     holds that own says this session didn't record;
//   - change:approved when the active version of a ready change has an
//     integration request, and no version at or above it was approved;
//   - change:integrated once, when a version landed: the active one, or
//     else the latest that did, as when another became active before the
//     check saw the landing;
//   - change:withdrawn once, when the change was withdrawn and its active
//     version didn't land;
//   - change:stale, change:rejected or change:failed when the integrator's
//     decision on a ready change's active version, which didn't land, is a
//     new one, or is on a later version than the last.
//
// A listing whose active version is below the highest the ledger holds was
// read before the one that recorded that version, and reports only a
// landing: its approval, outcome or withdrawal is old news, and changes
// nothing.
//
// A change the ledger holds nothing for is recorded as it stands, and
// returns none: a first look at a session's changes reports no history.
// As with Ledger.record, the file stays locked from reading it to renaming
// its replacement in, so two dashboards never both report a transition. An
// error means the marks are held in memory but couldn't be saved, or were
// saved without the lock.
func (l *ChangeLedger) Observe(repo string, ch valley.Change, own func(valley.Version) bool) ([]ChangeTransition, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, lockErr := l.lock()
	defer unlock()
	loadErr := l.mergeFromDisk()

	key := changeLedgerKey(repo, ch.ChangeID)
	e := l.changes[key]
	first := e == nil
	if first {
		e = &changeEntry{}
		l.changes[key] = e
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var out []ChangeTransition
	changed := false
	mark := func(field **ChangeMark, m ChangeMark, t ChangeTransition) {
		m.At = now
		*field = &m
		changed = true
		if !first && t.Type != "" {
			out = append(out, t)
		}
	}

	versions := append([]valley.Version(nil), ch.Versions...)
	sort.SliceStable(versions, func(i, j int) bool { return versions[i].N < versions[j].N })
	for _, v := range versions {
		if e.Version != nil && v.N <= e.Version.Version {
			continue
		}
		t := ChangeTransition{Version: v}
		if !first && !own(v) {
			t.Type = event.ChangeNewVersion
		}
		mark(&e.Version, ChangeMark{Version: v.N, Head: v.Head}, t)
	}

	active, ok := integration.ActiveVersion(ch)
	if !ok {
		return out, l.saved(changed, lockErr, loadErr)
	}
	// Only a landing is news from an older listing: a version that landed
	// stays landed.
	superseded := active.N < e.Version.Version
	if !superseded && ch.State == valley.StateReady && active.Request != nil && (e.Approved == nil || e.Approved.Version < active.N) {
		mark(&e.Approved, ChangeMark{Version: active.N, Head: active.Head},
			ChangeTransition{Type: event.ChangeApproved, Version: active})
	}
	if v, ok := landedVersion(versions, ch.Active); ok && e.Landed == nil {
		commit := v.Head
		if v.LandedCommit != nil && *v.LandedCommit != "" {
			commit = *v.LandedCommit
		}
		mark(&e.Landed, ChangeMark{Version: v.N, Head: v.Head, Commit: commit},
			ChangeTransition{Type: event.ChangeIntegrated, Version: v, Commit: commit})
	}
	switch {
	case landed(active), superseded:
	case ch.State == valley.StateWithdrawn:
		if e.Withdrawn == nil {
			mark(&e.Withdrawn, ChangeMark{Version: active.N, Head: active.Head},
				ChangeTransition{Type: event.ChangeWithdrawn, Version: active})
		}
	case ch.State == valley.StateReady:
		d := outcomeOf(active)
		if d != "" && (e.Outcome == nil || e.Outcome.Version < active.N ||
			e.Outcome.Version == active.N && e.Outcome.Decision != d) {
			mark(&e.Outcome, ChangeMark{Version: active.N, Head: active.Head, Decision: d},
				ChangeTransition{Type: outcomeEvents[d], Version: active})
		}
	}
	return out, l.saved(changed, lockErr, loadErr)
}

// landed reports whether the valley says v landed.
func landed(v valley.Version) bool {
	return v.Landing == valley.Landed || v.Landing == valley.LandedRecorded
}

// landedVersion is the version among versions that landed: the active one
// if it did, else the latest that did.
func landedVersion(versions []valley.Version, active int) (valley.Version, bool) {
	var found valley.Version
	ok := false
	for _, v := range versions {
		if !landed(v) {
			continue
		}
		if v.N == active {
			return v, true
		}
		if !ok || v.N > found.N {
			found, ok = v, true
		}
	}
	return found, ok
}

// outcomeOf is what the integrator decided on v's request, for a version
// that hasn't landed: stale, rejected or failed; uncertain when the valley
// has no landing evidence for a consumed or failed request, or records a
// decision or landing klaus doesn't know; "" when there is none yet.
func outcomeOf(v valley.Version) string {
	switch {
	case v.Landing == valley.Uncertain:
		return "uncertain"
	case v.Outcome != nil:
		d := strings.ToLower(v.Outcome.Decision)
		if outcomeEvents[d] != "" {
			return d
		}
		return "uncertain"
	case v.Landing != valley.NotLanded:
		return "uncertain"
	}
	return ""
}

// saved writes the ledger when Observe changed it, and joins the errors.
// The caller holds l.mu and the file lock.
func (l *ChangeLedger) saved(changed bool, errs ...error) error {
	if changed {
		errs = append(errs, l.save())
	}
	return errors.Join(errs...)
}

// lock takes an exclusive lock shared with every process using the ledger
// file, as Ledger.lock does.
func (l *ChangeLedger) lock() (unlock func(), err error) {
	f, err := os.OpenFile(l.path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}, fmt.Errorf("locking %s: %w", f.Name(), err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, nil
}

// mergeFromDisk adds the marks in the ledger file to the ones in memory,
// keeping the later of two: the higher version, or for an outcome on the
// same version, the one seen last. Every write merges the file first, so
// the file is never older than what a ledger holds, and wins a tie. The
// caller holds l.mu and the file lock, except in LoadChangeLedger.
func (l *ChangeLedger) mergeFromDisk() error {
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var disk map[string]*changeEntry
	if err := json.Unmarshal(data, &disk); err != nil {
		return fmt.Errorf("parsing %s: %w", l.path, err)
	}
	for key, d := range disk {
		if d == nil {
			continue
		}
		e := l.changes[key]
		if e == nil {
			e = &changeEntry{}
			l.changes[key] = e
		}
		later(&e.Version, d.Version)
		later(&e.Approved, d.Approved)
		later(&e.Landed, d.Landed)
		later(&e.Outcome, d.Outcome)
		later(&e.Withdrawn, d.Withdrawn)
	}
	return nil
}

// later sets *m to d unless *m is the later mark.
func later(m **ChangeMark, d *ChangeMark) {
	switch {
	case d == nil:
	case *m == nil, d.Version > (*m).Version, d.Version == (*m).Version && !d.seen().Before((*m).seen()):
		*m = d
	}
}

// seen is when the mark was first seen; the zero time if that can't be read.
func (m *ChangeMark) seen() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, m.At)
	return t
}

// save writes the ledger through a temporary file in the same directory, so
// a crash mid-write leaves the previous ledger in place.
func (l *ChangeLedger) save() error {
	data, err := json.MarshalIndent(l.changes, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(l.path), ".change-transitions-*")
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

// Package checksfix dispatches runs to fix the required checks the valley
// host's check worker failed on ready changes klaus owns, as the pipeline
// dispatches a fix agent for a PR's failing CI.
package checksfix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/restack"
	"github.com/patflynn/klaus/internal/valley"
)

// LedgerFile is the ledger's file name in a session directory.
const LedgerFile = "checks-dispatches.json"

// MaxAttempts is how many fix runs klaus dispatches for a change before it
// stops, as the pipeline's maxFixAttempts is for a PR's CI. Checks passing
// start the count over.
const MaxAttempts = 3

// maxHeads is how many of a change's failed heads the ledger remembers.
const maxHeads = 16

// Entry is what klaus did about one change's failed checks.
type Entry struct {
	Repo     string `json:"repo"`
	ChangeID string `json:"change_id"`
	Handle   string `json:"handle"`
	// Heads are the failed heads klaus acted on, latest last: each is
	// acted on once, whatever came of it.
	Heads []string `json:"heads"`
	// Attempts counts the fix runs that left the change's next version
	// still failing.
	Attempts int `json:"attempts"`
	// RunID, Head and At are the latest dispatch's run, the head it was
	// for, and when. RunID is cleared when checks pass.
	RunID string `json:"run_id,omitempty"`
	Head  string `json:"head,omitempty"`
	At    string `json:"at,omitempty"`
	// Stopped is set once the attempts are spent, until checks pass.
	Stopped bool `json:"stopped,omitempty"`
	// Error is why the latest launch failed, if it did.
	Error string `json:"error,omitempty"`
}

// Ledger is the session's record of fix dispatches, keyed by repo and
// change ID.
type Ledger struct {
	path    string
	entries map[string]*Entry
}

// LoadLedger reads the ledger in the session directory dir; a missing file
// is an empty ledger.
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

// Entry returns the ledger's entry for change id in repo, or nil.
func (l *Ledger) Entry(repo, id string) *Entry { return l.entries[repo+"/"+id] }

// Lock takes the ledger's lock in the session directory dir, which a
// pass that acts holds from reading the ledger to saving it.
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

// save writes the ledger through a temporary file, so a crash mid-write
// leaves the previous one in place.
func (l *Ledger) save() error {
	data, err := json.MarshalIndent(l.entries, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(l.path), ".checks-dispatches-*")
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

// Launch is a fix run to launch, in Dir: klaus launch [--repo Project]
// --change ChangeID --run-id RunID --expect-owner Owner --expect-branch
// Branch --expect-head Head --reason checks-failed Prompt.
type Launch struct {
	Dir, Project, ChangeID, Prompt string
	RunID                          string
	Owner, Branch, Head            string
}

// Deps are what a pass reads and does.
type Deps struct {
	List      func(ctx context.Context, dir string) (*valley.Changes, error)
	Principal func(ctx context.Context, dir string) string
	NewRunID  func() (string, error)
	Launch    func(ctx context.Context, l Launch) error
	// Running reports whether a run, in any session, is working on the
	// change or its branch.
	Running func(k restack.Key, branch string) bool
	Now     func() time.Time
}

// Pass reads the checks of the ready changes klaus owns in repos, on
// listed, or with nil, listing each repo, and with act dispatches a fix
// run for each failed head it hasn't acted on, saving what it did in l.
// It returns a line per change whose checks failed. A change is klaus's
// when its record's owner is the repo's principal (valley.principal). A
// change with no checks listed, or pending ones, is left alone.
func Pass(ctx context.Context, d Deps, l *Ledger, repos []restack.Repo, listed *valley.Changes, act bool) []string {
	var out []string
	for _, r := range repos {
		vr, ok := repoOf(ctx, d, r, listed)
		if !ok {
			continue
		}
		principal := d.Principal(ctx, r.Dir)
		if principal == "" {
			continue
		}
		for _, ch := range vr.Changes {
			if line := handle(ctx, d, l, r, ch, principal, act); line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

// repoOf is repo r as listed, or as the valley lists it now.
func repoOf(ctx context.Context, d Deps, r restack.Repo, listed *valley.Changes) (valley.Repo, bool) {
	if listed == nil {
		var err error
		if listed, err = d.List(ctx, r.Dir); err != nil {
			return valley.Repo{}, false
		}
	}
	for _, vr := range listed.Repos {
		if vr.Repo == r.Name && vr.Err() == "" {
			return vr, true
		}
	}
	return valley.Repo{}, false
}

// handle acts on one change's checks and says what came of them.
func handle(ctx context.Context, d Deps, l *Ledger, r restack.Repo, ch valley.Change, principal string, act bool) string {
	if ch.State != valley.StateReady || ch.Owner != principal {
		return ""
	}
	v, ok := integration.ActiveVersion(ch)
	if !ok || v.Checks == nil || v.Landing == valley.Landed || v.Landing == valley.LandedRecorded {
		return ""
	}
	key := r.Name + "/" + ch.ChangeID
	e := l.entries[key]
	name := ch.Handle
	if name == "" {
		name = ch.ChangeID
	}
	switch v.Checks.State {
	case valley.ChecksPassed:
		// Checks passing start the count over, as CI passing does.
		if act && e != nil && (e.Attempts > 0 || e.Stopped || e.RunID != "") {
			e.Attempts, e.Stopped, e.RunID = 0, false, ""
			l.save() //nolint:errcheck // a later pass resets it again
		}
		return ""
	case valley.ChecksFailed:
	default:
		return ""
	}
	label := fmt.Sprintf("%s %s v%d checks failed (%s)", r.Name, name, v.N, strings.Join(v.Checks.FailedNames(), ", "))
	if e != nil && slices.Contains(e.Heads, v.Head) {
		return label + ": " + e.said(v.Head)
	}
	if d.Running(restack.Key{Repo: r.Name, ChangeID: ch.ChangeID}, v.Branch) {
		return label + ": a run is working on the change"
	}
	attempts := 0
	if e != nil {
		attempts = e.Attempts
		if e.RunID != "" {
			attempts++ // the last fix run left this version failing
		}
	}
	if !act {
		if attempts >= MaxAttempts {
			return label + fmt.Sprintf(": stopped after %d fix runs", attempts)
		}
		return label + ": a fix run is due"
	}
	if e == nil {
		e = &Entry{Repo: r.Name, ChangeID: ch.ChangeID}
		l.entries[key] = e
	}
	e.Handle = name
	e.Attempts = attempts
	e.Heads = append(e.Heads, v.Head)
	if len(e.Heads) > maxHeads {
		e.Heads = e.Heads[len(e.Heads)-maxHeads:]
	}
	e.Head, e.Error, e.RunID = v.Head, "", ""
	e.At = d.Now().UTC().Format(time.RFC3339)
	if attempts >= MaxAttempts {
		e.Stopped = true
		l.save() //nolint:errcheck // the head is acted on in memory either way
		return label + ": " + e.said(v.Head)
	}
	runID, err := d.NewRunID()
	if err != nil {
		e.Error = err.Error()
		l.save() //nolint:errcheck
		return label + ": " + e.said(v.Head)
	}
	e.RunID = runID
	// Record the dispatch before launching it: a pass that can't record
	// what it does dispatches nothing.
	if err := l.save(); err != nil {
		return label + ": not dispatched: saving the ledger: " + err.Error()
	}
	err = d.Launch(ctx, Launch{
		Dir: r.Dir, Project: r.Project, ChangeID: ch.ChangeID, RunID: runID,
		Owner: principal, Branch: v.Branch, Head: v.Head, Prompt: Prompt(r.Name, ch, v),
	})
	if err != nil {
		// A failed launch is no fix attempt.
		e.Error, e.RunID = err.Error(), ""
		l.save() //nolint:errcheck
	}
	return label + ": " + e.said(v.Head)
}

// said is what the entry says klaus did about failed head.
func (e *Entry) said(head string) string {
	switch {
	case e.Head != head:
		return "acted on before"
	case e.Stopped:
		return fmt.Sprintf("stopped after %d fix runs", e.Attempts)
	case e.Error != "":
		return "not dispatched: " + e.Error
	}
	return fmt.Sprintf("fix run %s dispatched (attempt %d of %d)", e.RunID, e.Attempts+1, MaxAttempts)
}

// plainName is a check name klaus passes on to a run's prompt.
var plainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,99}$`)

// maxWhy bounds the worker's reason a prompt quotes for a check.
const maxWhy = 300

// Prompt is the brief of a run that fixes the failed checks of version v of
// change ch in repo.
func Prompt(repo string, ch valley.Change, v valley.Version) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The valley host's check worker failed required checks of change %s (%s) v%d in %s, at head %s on branch %s:\n\n",
		ch.Handle, ch.ChangeID, v.N, repo, v.Head, v.Branch)
	failed := v.Checks.Failed()
	sort.SliceStable(failed, func(i, j int) bool { return failed[i].Name < failed[j].Name })
	for _, c := range failed {
		name := c.Name
		if !plainName.MatchString(name) {
			name = "(a check with an unprintable name)"
		}
		why := strings.Join(strings.Fields(c.Why), " ")
		if len(why) > maxWhy {
			why = why[:maxWhy] + "..."
		}
		if why == "" {
			why = "no reason given"
		}
		fmt.Fprintf(&b, "  - %s: the worker says %q\n", name, why)
	}
	fmt.Fprintf(&b, `
Read the worker's build output for each failed check with
'valley telemetry --json read --source worker --logs', keeping the records
for change ID %s and the check's name. A read returns at most 200 records,
and its last line carries a cursor: pass it with --since to read the next
page, until you have the check's whole run.

Fix the failures by amending the change's single commit (git commit
--amend); don't add commits. Before you push, build and run every check
'valley checks --names origin/main HEAD' lists, and push only once each
passes.`, ch.ChangeID)
	return b.String()
}

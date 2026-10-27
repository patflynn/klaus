package restack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// Repo is a valley repo the session's runs deliver to, and a clone to read
// it in and launch from.
type Repo struct {
	Name    string // the valley's name for it (integration.RepoName)
	Dir     string // a clone whose origin it is
	Project string // what launch --repo names it by; "" to launch from Dir
}

// Repos returns the valley repos the runs deliver to: a run's repo counts
// once finalize marked a version ready there, launch returned its change
// to draft, or klaus track added it. Each is read in a run's clone, or else
// in root, the repo the caller runs in; a listing names its repo, and one
// that names another is skipped.
func Repos(states []*run.State, root string) []Repo {
	byName := make(map[string]Repo)
	for _, s := range states {
		if s.Valley == nil && s.Unready == nil && !integration.TracksChange(s) {
			continue
		}
		name := integration.RepoName(s)
		if name == "" {
			continue
		}
		if s.CloneDir != nil && *s.CloneDir != "" {
			if fi, err := os.Stat(*s.CloneDir); err == nil && fi.IsDir() {
				r := Repo{Name: name, Dir: *s.CloneDir}
				if s.TargetRepo != nil {
					r.Project = *s.TargetRepo
				}
				if prev, ok := byName[name]; !ok || prev.Project == "" {
					byName[name] = r
				}
				continue
			}
		}
		if _, ok := byName[name]; !ok && root != "" {
			byName[name] = Repo{Name: name, Dir: root}
		}
	}
	out := make([]Repo, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Launch is a resolution run to launch, in Dir: klaus launch [--repo
// Project] --change ChangeID --run-id RunID --expect-owner Owner
// --expect-branch Branch --expect-head Head --expect-draft --reason
// restack-conflict Prompt. Launch checks the change still stands as klaus
// checked it, and starts no worker unless it does.
type Launch struct {
	Dir, Project, ChangeID, Prompt string
	RunID                          string // the run's ID, which klaus records before launching it
	Owner, Branch, Head            string // the change as klaus checked it: its owner, branch and head, a draft
}

// ErrNotStarted is a launch error that says no worker started: launch
// failed before starting one.
var ErrNotStarted = errors.New("launch started no worker")

// Deps are what a pass reads and does. Production uses Default's; tests
// replace them.
type Deps struct {
	List       func(ctx context.Context, dir string) (*valley.Changes, error)
	Principal  func(ctx context.Context, dir string) string
	OriginRefs func(ctx context.Context, dir string, refs ...string) (map[string]string, error)
	Unready    func(ctx context.Context, dir, changeID string) error
	// NewRunID names a run before it is launched.
	NewRunID func() (string, error)
	// Launch launches a resolution run under l.RunID. An error wrapping
	// ErrNotStarted says no worker started; any other may come after one
	// did.
	Launch func(ctx context.Context, l Launch) error
	// Started reports whether a run by an ID is recorded, in any session.
	Started func(runID string) bool
	// Running reports whether a run, in any session, is working on the
	// change or its branch.
	Running func(k Key, branch string) bool
	// WorkerGone reports whether klaus can prove that no worker of a run
	// by an ID runs, nor is about to start, from the clone at dir. Any
	// doubt is false.
	WorkerGone func(ctx context.Context, dir, runID string) bool
	// Emit records an event under a run ID.
	Emit func(runID, typ string, data map[string]interface{})
	Now  func() time.Time
}

// Default returns the Deps that run valley and git. launch, started,
// running, gone and emit are the caller's.
func Default(launch func(context.Context, Launch) error, started func(string) bool, running func(Key, string) bool,
	gone func(context.Context, string, string) bool, emit func(string, string, map[string]interface{})) Deps {
	return Deps{
		List:       valley.ListRepo,
		Principal:  valley.Principal,
		OriginRefs: valley.OriginRefs,
		Unready: func(ctx context.Context, dir, id string) error {
			_, err := valley.Unready(ctx, dir, id)
			return err
		},
		NewRunID:   run.GenID,
		Launch:     launch,
		Started:    started,
		Running:    running,
		WorkerGone: gone,
		Emit:       emit,
		Now:        time.Now,
	}
}

// RunningOn returns a Running that reports whether any of states, judged
// by td, is running on a change (its change ID in its repo) or its branch.
func RunningOn(states []*run.State, td run.TmuxDeps) func(Key, string) bool {
	return func(k Key, branch string) bool {
		for _, s := range states {
			if s == nil || integration.RepoName(s) != k.Repo {
				continue
			}
			if (s.ChangeID == k.ChangeID || branch != "" && s.Branch == branch) && s.IsAgentRunningWith(td) {
				return true
			}
		}
		return false
	}
}

// The states of a View.
const (
	StateDue        = "due"        // a run would be dispatched (a pass that doesn't act)
	StateDispatched = "dispatched" // a run was dispatched for the conflict
	StateLaunching  = "launching"  // klaus launched a run and can't yet tell whether it started
	StateBusy       = "busy"       // a run is working on the change
	StateCooldown   = "cooldown"   // a run was dispatched within the cooldown
	StateStopped    = "stopped"    // the attempts are spent
	StateRefused    = "refused"    // klaus didn't launch: Detail says why
	StateHeld       = "held"       // klaus leaves the conflict alone: Detail says why
	StateStale      = "stale"      // the report's inputs moved when klaus read them again
)

// View is where a change's restack conflict stands after a pass.
type View struct {
	Key
	Handle    string
	Version   int
	Head      string
	TargetTip string
	State     string
	RunID     string // the latest resolution run
	Attempts  int
	Detail    string
}

// Name is how a view names its change: repo and handle, or the change ID.
func (v View) Name() string {
	if v.Handle != "" {
		return v.Repo + " " + v.Handle
	}
	return v.Repo + " " + v.ChangeID
}

// Summary says, in a line, where the conflict stands.
func (v View) Summary() string {
	switch v.State {
	case StateDue:
		return "a resolution run is due"
	case StateDispatched:
		return fmt.Sprintf("resolution run %s dispatched (attempt %d of %d)", v.RunID, v.Attempts, MaxAttempts)
	case StateLaunching:
		if v.Detail != "" {
			return fmt.Sprintf("resolution run %s (attempt %d of %d) may have started its worker: %s; klaus waits until it can tell none runs", v.RunID, v.Attempts, MaxAttempts, v.Detail)
		}
		return fmt.Sprintf("launching resolution run %s (attempt %d of %d)", v.RunID, v.Attempts, MaxAttempts)
	case StateBusy:
		return "a run is working on the change"
	case StateCooldown:
		return fmt.Sprintf("waiting out the cooldown after run %s (attempt %d of %d)", v.RunID, v.Attempts, MaxAttempts)
	case StateStopped:
		return fmt.Sprintf("stopped after %d resolution runs; resolve it by hand", v.Attempts)
	case StateRefused:
		return "not dispatched: " + v.Detail
	case StateHeld:
		return "left alone: " + v.Detail
	case StateStale:
		return "skipped, stale: " + v.Detail
	}
	return v.State
}

// String is the view as klaus status and klaus restack-conflicts print it.
func (v View) String() string {
	return fmt.Sprintf("%s v%d: conflict rebasing onto main at %s; %s", v.Name(), v.Version, short(v.TargetTip), v.Summary())
}

// Pass reads each repo's restack reports and returns a view of every
// conflict on a change klaus owns: the current conflicts, and the ones it
// acted on whose change still stands at the head it acted on. It reads
// them in listed, a listing already read, as the dashboard's integration
// check reads one for every repo; with none, it lists each repo itself. With
// act, in each repo it first settles the launches the ledger holds pending
// (reconcile) and drops the entries of changes that landed or were
// withdrawn (prune), then dispatches what Decide says to, and records what
// it did in the ledger; without, it only reports, leaving the ledger and
// the valley as they were. A repo the valley can't be asked about is
// skipped.
func Pass(ctx context.Context, d Deps, l *Ledger, repos []Repo, listed *valley.Changes, act bool) []View {
	var views []View
	for _, r := range repos {
		if act {
			d.reconcile(ctx, l, r)
		}
		vr, ok := repoIn(listed, r)
		if listed == nil {
			vr, ok = listRepo(ctx, d, r)
		}
		if !ok {
			continue
		}
		if act {
			d.prune(l, vr)
		}
		principal := d.Principal(ctx, r.Dir)
		conflicts, clean := Scan(vr, principal)
		if act {
			for _, k := range clean {
				d.reset(ctx, l, r, k)
			}
		}
		seen := make(map[Key]bool)
		for _, c := range conflicts {
			seen[c.Key] = true
			views = append(views, d.handle(ctx, l, r, c, act))
		}
		views = append(views, d.actedViews(vr, principal, l, seen, act)...)
	}
	return views
}

// reconcile settles the launches the ledger holds pending in repo r: one a
// pass left when it ended before its launch returned, or one whose launch
// failed leaving klaus unsure whether its worker started. A launch whose
// run is recorded is a dispatch. One whose worker klaus can prove gone
// (WorkerGone) ended without a run klaus can vouch for, keeping its
// attempt and its cooldown since a worker may have run; klaus says it
// stopped short. The rest stay pending, their changes busy, for as long as
// klaus can't tell: the passing of time proves nothing.
func (d Deps) reconcile(ctx context.Context, l *Ledger, r Repo) {
	for _, e := range l.sorted() {
		a := e.Pending()
		if e.Repo != r.Name || a == nil {
			continue
		}
		switch {
		case d.started(a.RunID):
			a.Pending = false
			if !d.save(l) {
				a.Pending = true
				continue
			}
			d.emitDispatched(e, a)
		case d.gone(ctx, r.Dir, a.RunID):
			failed := a.Refused
			a.Pending = false
			a.Refused = fmt.Sprintf("klaus lost sight of its launch of run %s, and no worker of it runs; the attempt still counts", a.RunID)
			if failed != "" {
				a.Refused = fmt.Sprintf("no worker of run %s runs, and the attempt still counts; %s", a.RunID, failed)
			}
			if !d.save(l) {
				a.Pending, a.Refused = true, failed
				continue
			}
			d.emitStalled(e, a, a.Refused)
		}
	}
}

// prune drops the entries of vr's changes that landed or were withdrawn:
// klaus dispatches nothing more for them. An entry with a launch pending
// stays until it is settled, and one whose change vr doesn't list stays.
func (d Deps) prune(l *Ledger, vr valley.Repo) {
	live := make(map[string]bool)
	listed := make(map[string]bool)
	for _, ch := range vr.Changes {
		listed[ch.ChangeID] = true
		active, ok := activeVersion(ch)
		landed := ok && (active.Landing == valley.Landed || active.Landing == valley.LandedRecorded)
		if ch.State != valley.StateWithdrawn && !landed {
			live[ch.ChangeID] = true
		}
	}
	var dropped []*Entry
	for _, e := range l.sorted() {
		if e.Repo == vr.Repo && listed[e.ChangeID] && !live[e.ChangeID] && e.Pending() == nil {
			l.drop(Key{Repo: e.Repo, ChangeID: e.ChangeID})
			dropped = append(dropped, e)
		}
	}
	if len(dropped) > 0 && !d.save(l) {
		for _, e := range dropped {
			l.put(e)
		}
	}
}

// reset gives a change its attempts back after a clean restack, once per
// clean report, and only while origin still holds that report: the
// listing may have been read before a pass that dispatched on a newer
// conflict report took the ledger, and a report origin no longer holds
// says nothing of the attempts made since.
func (d Deps) reset(ctx context.Context, l *Ledger, r Repo, k Clean) {
	e := l.Entry(k.Key)
	if e == nil || e.Attempts == 0 && e.Stopped == "" || e.Clean == k.Report || e.Pending() != nil {
		return
	}
	ref := ReportRef(k.Target, k.ChangeID)
	refs, err := d.OriginRefs(ctx, r.Dir, ref)
	if err != nil || refs[ref] != k.Report {
		return
	}
	attempts, stopped, clean := e.Attempts, e.Stopped, e.Clean
	e.Attempts, e.Stopped, e.Clean = 0, "", k.Report
	if !d.save(l) { // a reset that isn't saved is made again next pass
		e.Attempts, e.Stopped, e.Clean = attempts, stopped, clean
	}
}

// listRepo lists a repo, and returns the listing's entry for it.
func listRepo(ctx context.Context, d Deps, r Repo) (valley.Repo, bool) {
	l, err := d.List(ctx, r.Dir)
	if err != nil {
		return valley.Repo{}, false
	}
	return repoIn(l, r)
}

// repoIn returns a listing's entry for a repo it could read.
func repoIn(l *valley.Changes, r Repo) (valley.Repo, bool) {
	if l == nil {
		return valley.Repo{}, false
	}
	for _, vr := range l.Repos {
		if vr.Repo == r.Name && vr.Err() == "" {
			return vr, true
		}
	}
	return valley.Repo{}, false
}

// actedViews returns a view of each change in vr that klaus acted on and
// that has no current conflict (seen), while it is unlanded, not
// withdrawn, and its latest report is one klaus acted on, at the head it
// acted on: after klaus returned it to draft, its report reads historical.
// Once no run is working on such a change and its attempts are spent, it
// is stopped, and with act klaus says so once.
func (d Deps) actedViews(vr valley.Repo, principal string, l *Ledger, seen map[Key]bool, act bool) []View {
	var views []View
	for _, ch := range vr.Changes {
		k := Key{Repo: vr.Repo, ChangeID: ch.ChangeID}
		e := l.Entry(k)
		if seen[k] || e == nil || ch.Owner != principal || ch.Restack == nil {
			continue
		}
		active, ok := activeVersion(ch)
		landed := active.Landing == valley.Landed || active.Landing == valley.LandedRecorded
		a := e.actionOnReport(ch.Restack.Commit, active.Head)
		if !ok || a == nil || ch.State == valley.StateWithdrawn || landed {
			continue
		}
		v := viewOf(k, ch.Handle, ch.Active, e, a)
		if e.Attempts >= MaxAttempts && e.Pending() == nil && !d.Running(k, active.Branch) {
			v.State, v.Detail = StateStopped, ""
			if act {
				d.stop(l, e, a)
			}
		}
		views = append(views, v)
	}
	return views
}

// viewOf is the view of what klaus did, a, about one of a change's
// conflicts, e its entry.
func viewOf(k Key, handle string, version int, e *Entry, a *Action) View {
	v := View{Key: k, Handle: handle, Version: version, Head: a.Head, TargetTip: a.TargetTip,
		RunID: a.RunID, Attempts: e.Attempts, State: StateDispatched}
	switch {
	case a.Pending:
		v.State, v.Detail = StateLaunching, a.Refused
	case a.Refused != "":
		v.State, v.Detail = StateRefused, a.Refused
	case e.Stopped != "":
		v.State = StateStopped
	}
	return v
}

// stop records that klaus stopped dispatching for a change whose attempts
// are spent, on the conflict a names, and says so once: only once it is
// recorded that it did.
func (d Deps) stop(l *Ledger, e *Entry, a *Action) {
	if e.Stopped != "" {
		return
	}
	e.Stopped = d.Now().UTC().Format(time.RFC3339)
	if !d.save(l) {
		e.Stopped = ""
		return
	}
	d.emitStalled(e, a, fmt.Sprintf("%d resolution runs did not resolve its restack conflicts", e.Attempts))
}

// handle decides about one conflict and, with act, carries the decision
// out.
func (d Deps) handle(ctx context.Context, l *Ledger, r Repo, c Conflict, act bool) View {
	e := l.Entry(c.Key)
	v := View{Key: c.Key, Handle: c.Handle, Version: c.Version, Head: c.Head, TargetTip: c.TargetTip}
	if e != nil {
		v.Attempts = e.Attempts
		if last := e.Last(); last != nil {
			v.RunID = last.RunID
		}
	}
	switch Decide(d.Now(), c, e, d.Running(c.Key, c.Branch)) {
	case Held:
		v.State, v.Detail = StateHeld, c.Held
	case Acted:
		v = viewOf(c.Key, c.Handle, c.Version, e, e.ActionOn(c.inputs()))
	case Busy:
		v.State = StateBusy
		if a := e.Pending(); a != nil {
			v = viewOf(c.Key, c.Handle, c.Version, e, a)
		}
	case Wait:
		v.State = StateCooldown
	case Stop:
		v.State = StateStopped
		if act {
			d.stop(l, e, &Action{Report: c.Report, Version: c.Version, Head: c.Head, TargetTip: c.TargetTip})
		}
	case Dispatch:
		if !act {
			v.State = StateDue
			break
		}
		return d.dispatch(ctx, l, r, c)
	}
	return v
}

// dispatch reads the change and origin again, returns the change to draft,
// and launches a resolution run, recording what it did. A report whose
// inputs moved is skipped and nothing is recorded: the next pass reads
// whatever replaced it. If the change can't be confirmed a draft, owned by
// klaus at the head and branch it checked, no run launches; and launch
// checks all of it again before it starts a worker.
//
// The ledger is written ahead: the inputs, the attempt and the run's ID,
// pending, are saved before klaus touches the change, and if they can't
// be, it does nothing. An attempt is given back only once klaus knows no
// worker started: it stopped before launching, or launch says it started
// none. A failed save later can then only make klaus do less, never act
// twice on one conflict or run past MaxAttempts, and a pass that ends
// mid-launch leaves the launch for the next to reconcile. So does a launch
// that fails after it may have started its worker, unless klaus can prove
// no worker of it runs (WorkerGone): until it can, the change is busy.
func (d Deps) dispatch(ctx context.Context, l *Ledger, r Repo, c Conflict) View {
	v := View{Key: c.Key, Handle: c.Handle, Version: c.Version, Head: c.Head, TargetTip: c.TargetTip}
	if e := l.Entry(c.Key); e != nil {
		v.Attempts = e.Attempts
	}
	if why := d.recheck(ctx, r, c); why != "" {
		v.State, v.Detail = StateStale, why
		return v
	}
	runID, err := d.NewRunID()
	if err != nil {
		v.State, v.Detail = StateRefused, "klaus couldn't name the run: "+err.Error()
		return v
	}

	e := l.Entry(c.Key)
	if e == nil {
		e = &Entry{Repo: c.Repo, ChangeID: c.ChangeID}
		l.put(e)
	}
	act := &Action{At: d.Now().UTC(), Report: c.Report, Version: c.Version, Head: c.Head, TargetTip: c.TargetTip,
		RunID: runID, Pending: true}
	prevActions, prevAttempts, prevHandle := e.Actions, e.Attempts, e.Handle
	e.Handle = c.Handle
	e.Attempts++ // counted up front, and given back once no worker started
	e.record(act)
	if !d.save(l) {
		e.Actions, e.Attempts, e.Handle = prevActions, prevAttempts, prevHandle
		v.State, v.Detail = StateRefused, "klaus couldn't record the dispatch in its ledger"
		return v
	}
	// refuse gives the attempt back: no worker started.
	refuse := func(why string) View {
		e.Attempts--
		act.RunID, act.Pending, act.Refused = "", false, why
		d.save(l)
		d.emitStalled(e, act, why)
		v.State, v.Detail, v.Attempts = StateRefused, why, e.Attempts
		return v
	}

	// The change must be a draft before a run edits it, so approve
	// refuses it meanwhile.
	if err := d.Unready(ctx, r.Dir, c.ChangeID); err != nil {
		return refuse("could not return the change to draft: " + err.Error())
	}
	vr, ok := listRepo(ctx, d, r)
	if !ok {
		return refuse("could not confirm the change is a draft: the valley couldn't be asked")
	}
	ch, found := changeIn(vr, c.ChangeID)
	active, hasActive := activeVersion(ch)
	switch {
	case !found:
		return refuse("could not confirm the change is a draft: the valley no longer lists it")
	case ch.State != valley.StateDraft:
		return refuse(fmt.Sprintf("the change is %s after valley unready, not a draft", ch.State))
	case ch.Owner != c.Owner:
		return refuse("the change's owner changed while klaus returned it to draft")
	case !hasActive || active.Head != c.Head || active.Branch != c.Branch:
		return refuse("the change's head moved while klaus returned it to draft")
	}

	prompt := Prompt(c)
	if prompt == "" {
		return refuse("its change ID or heads don't read as klaus expects")
	}
	err = d.Launch(ctx, Launch{Dir: r.Dir, Project: r.Project, ChangeID: c.ChangeID, Prompt: prompt,
		RunID: runID, Owner: c.Owner, Branch: c.Branch, Head: c.Head})
	switch {
	case err == nil || d.started(runID):
		act.Pending = false
		d.save(l)
		d.emitDispatched(e, act)
		v.State, v.RunID, v.Attempts = StateDispatched, runID, e.Attempts
	case errors.Is(err, ErrNotStarted):
		return refuse("launch failed, and the change is left a draft: " + firstLine(err.Error()))
	default:
		// Launch may have started the worker and failed after: the
		// attempt and the cooldown stand, and the launch stays pending
		// until klaus can prove no worker of it runs.
		act.Refused = "launch failed: " + firstLine(err.Error())
		d.save(l)
		d.reconcile(ctx, l, r)
		v = viewOf(c.Key, c.Handle, c.Version, e, act)
	}
	return v
}

// recheck reads the change and origin's refs again, and says why the
// conflict no longer stands as the report says, or "" when it does.
func (d Deps) recheck(ctx context.Context, r Repo, c Conflict) string {
	vr, ok := listRepo(ctx, d, r)
	if !ok {
		return "the valley couldn't be asked again"
	}
	again, _ := Scan(vr, d.Principal(ctx, r.Dir))
	same := false
	for _, a := range again {
		if a.Key == c.Key && a.Report == c.Report && a.Head == c.Head && a.TargetTip == c.TargetTip && a.Held == "" {
			same = true
		}
	}
	if !same {
		return "the valley's view of the change changed"
	}
	refs, err := d.OriginRefs(ctx, r.Dir, Refs(c)...)
	if err != nil {
		return err.Error()
	}
	return Stale(c, refs)
}

func changeIn(vr valley.Repo, id string) (valley.Change, bool) {
	for _, ch := range vr.Changes {
		if ch.ChangeID == id {
			return ch, true
		}
	}
	return valley.Change{}, false
}

// save writes the ledger, and reports whether it could.
func (d Deps) save(l *Ledger) bool {
	if err := l.save(); err != nil {
		slog.Warn("saving the restack ledger", "err", err)
		return false
	}
	return true
}

func (d Deps) emit(runID, typ string, data map[string]interface{}) {
	if d.Emit != nil {
		d.Emit(runID, typ, data)
	}
}

// gone reports whether klaus can prove no worker of run runID runs.
func (d Deps) gone(ctx context.Context, dir, runID string) bool {
	return runID != "" && d.WorkerGone != nil && d.WorkerGone(ctx, dir, runID)
}

// started reports whether a run by runID is recorded.
func (d Deps) started(runID string) bool {
	return runID != "" && d.Started != nil && d.Started(runID)
}

// emitDispatched says klaus dispatched a's run, under the run's ID.
func (d Deps) emitDispatched(e *Entry, a *Action) {
	d.emit(a.RunID, event.ChangeRestackDispatched, map[string]interface{}{
		"repo": e.Repo, "change_id": e.ChangeID, "handle": e.Handle, "version": a.Version,
		"head": a.Head, "target_tip": a.TargetTip, "report": a.Report,
		"attempt": e.Attempts, "max_attempts": MaxAttempts,
	})
}

// emitStalled says klaus stopped short of resolving a change's conflict,
// the one a names. It goes out under the change ID, which no run has.
func (d Deps) emitStalled(e *Entry, a *Action, reason string) {
	data := map[string]interface{}{"repo": e.Repo, "change_id": e.ChangeID, "handle": e.Handle, "reason": reason}
	if a != nil {
		data["version"], data["head"], data["target_tip"], data["report"] = a.Version, a.Head, a.TargetTip, a.Report
		if a.RunID != "" {
			data["run_id"] = a.RunID
		}
	}
	d.emit(e.ChangeID, event.ChangeRestackStalled, data)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:199]) + "…"
	}
	return s
}

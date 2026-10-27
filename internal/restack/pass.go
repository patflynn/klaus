package restack

import (
	"context"
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

// Launch is a resolution run to launch: klaus launch [--repo Project]
// --change ChangeID --reason restack-conflict Prompt, in Dir.
type Launch struct {
	Dir, Project, ChangeID, Prompt string
}

// Deps are what a pass reads and does. Production uses Default's; tests
// replace them.
type Deps struct {
	List       func(ctx context.Context, dir string) (*valley.Changes, error)
	Principal  func(ctx context.Context, dir string) string
	OriginRefs func(ctx context.Context, dir string, refs ...string) (map[string]string, error)
	Unready    func(ctx context.Context, dir, changeID string) error
	// Launch launches a resolution run and returns its run ID.
	Launch func(ctx context.Context, l Launch) (string, error)
	// Running reports whether a run, in any session, is working on the
	// change or its branch.
	Running func(k Key, branch string) bool
	// Emit records an event under a run ID.
	Emit func(runID, typ string, data map[string]interface{})
	Now  func() time.Time
}

// Default returns the Deps that run valley and git. launch, running and
// emit are the caller's.
func Default(launch func(context.Context, Launch) (string, error), running func(Key, string) bool, emit func(string, string, map[string]interface{})) Deps {
	return Deps{
		List:       valley.ListRepo,
		Principal:  valley.Principal,
		OriginRefs: valley.OriginRefs,
		Unready: func(ctx context.Context, dir, id string) error {
			_, err := valley.Unready(ctx, dir, id)
			return err
		},
		Launch:  launch,
		Running: running,
		Emit:    emit,
		Now:     time.Now,
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
// act it dispatches what Decide says to, and records what it did in the
// ledger; without, it only reports, leaving the ledger and the valley as
// they were. A repo the valley can't be asked about is skipped.
func Pass(ctx context.Context, d Deps, l *Ledger, repos []Repo, listed *valley.Changes, act bool) []View {
	var views []View
	for _, r := range repos {
		vr, ok := repoIn(listed, r)
		if listed == nil {
			vr, ok = listRepo(ctx, d, r)
		}
		if !ok {
			continue
		}
		principal := d.Principal(ctx, r.Dir)
		conflicts, clean := Scan(vr, principal)
		if act {
			for _, k := range clean {
				if e := l.Entry(k); e != nil && (e.Attempts > 0 || e.Stopped != "") {
					e.Attempts, e.Stopped = 0, ""
					d.save(l) // a reset that isn't saved is made again next pass
				}
			}
		}
		seen := make(map[Key]bool)
		for _, c := range conflicts {
			seen[c.Key] = true
			views = append(views, d.handle(ctx, l, r, c, act))
		}
		views = append(views, actedViews(vr, principal, l, seen)...)
	}
	return views
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
// that has no current conflict (seen), while it is still at the head klaus
// acted on, unlanded and not withdrawn, and its latest report is the one
// acted on: after klaus returned it to draft, its report reads historical.
func actedViews(vr valley.Repo, principal string, l *Ledger, seen map[Key]bool) []View {
	var views []View
	for _, ch := range vr.Changes {
		k := Key{Repo: vr.Repo, ChangeID: ch.ChangeID}
		e := l.Entry(k)
		if seen[k] || e == nil || e.Last == nil || ch.Owner != principal || ch.Restack == nil || ch.Restack.Commit != e.Last.Report {
			continue
		}
		active, ok := activeVersion(ch)
		landed := active.Landing == valley.Landed || active.Landing == valley.LandedRecorded
		if !ok || active.Head != e.Last.Head || ch.State == valley.StateWithdrawn || landed {
			continue
		}
		views = append(views, viewOf(k, ch.Handle, ch.Active, e))
	}
	return views
}

// viewOf is the view of what the ledger says klaus last did for a change.
func viewOf(k Key, handle string, version int, e *Entry) View {
	v := View{Key: k, Handle: handle, Version: version, Head: e.Last.Head, TargetTip: e.Last.TargetTip,
		RunID: e.Last.RunID, Attempts: e.Attempts, State: StateDispatched}
	switch {
	case e.Last.Refused != "":
		v.State, v.Detail = StateRefused, e.Last.Refused
	case e.Stopped != "":
		v.State = StateStopped
	}
	return v
}

// handle decides about one conflict and, with act, carries the decision
// out.
func (d Deps) handle(ctx context.Context, l *Ledger, r Repo, c Conflict, act bool) View {
	e := l.Entry(c.Key)
	v := View{Key: c.Key, Handle: c.Handle, Version: c.Version, Head: c.Head, TargetTip: c.TargetTip}
	if e != nil {
		v.Attempts = e.Attempts
		if e.Last != nil {
			v.RunID = e.Last.RunID
		}
	}
	switch Decide(d.Now(), c, e, d.Running(c.Key, c.Branch)) {
	case Held:
		v.State, v.Detail = StateHeld, c.Held
	case Acted:
		v = viewOf(c.Key, c.Handle, c.Version, e)
	case Busy:
		v.State = StateBusy
	case Wait:
		v.State = StateCooldown
	case Stop:
		v.State = StateStopped
		if act && e.Stopped == "" {
			// Said once: only once it is recorded that it was.
			e.Stopped = d.Now().UTC().Format(time.RFC3339)
			if d.save(l) {
				d.emitStalled(c, fmt.Sprintf("%d resolution runs did not resolve its restack conflicts", e.Attempts))
			} else {
				e.Stopped = ""
			}
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
// whatever replaced it. If the change can't be confirmed a draft, no run
// launches.
//
// The ledger is written ahead: the inputs and the attempt are saved before
// klaus touches the change, and if they can't be, it does nothing. A
// failed save later can then only make klaus do less, never act twice on
// one conflict or run past MaxAttempts.
func (d Deps) dispatch(ctx context.Context, l *Ledger, r Repo, c Conflict) View {
	v := View{Key: c.Key, Handle: c.Handle, Version: c.Version, Head: c.Head, TargetTip: c.TargetTip}
	if e := l.Entry(c.Key); e != nil {
		v.Attempts = e.Attempts
	}
	if why := d.recheck(ctx, r, c); why != "" {
		v.State, v.Detail = StateStale, why
		return v
	}

	e := l.Entry(c.Key)
	if e == nil {
		e = &Entry{Repo: c.Repo, ChangeID: c.ChangeID}
		l.put(e)
	}
	act := &Action{At: d.Now().UTC(), Report: c.Report, Head: c.Head, TargetTip: c.TargetTip}
	prevLast, prevAttempts, prevHandle := e.Last, e.Attempts, e.Handle
	e.Last, e.Handle = act, c.Handle
	e.Attempts++ // counted up front, and given back if no run launches
	if !d.save(l) {
		e.Last, e.Attempts, e.Handle = prevLast, prevAttempts, prevHandle
		v.State, v.Detail = StateRefused, "klaus couldn't record the dispatch in its ledger"
		return v
	}
	refuse := func(why string) View {
		e.Attempts--
		act.Refused = why
		d.save(l)
		d.emitStalled(c, why)
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
	case !hasActive || active.Head != c.Head:
		return refuse("the change's head moved while klaus returned it to draft")
	}

	prompt := Prompt(c)
	if prompt == "" {
		return refuse("its change ID or heads don't read as klaus expects")
	}
	runID, err := d.Launch(ctx, Launch{Dir: r.Dir, Project: r.Project, ChangeID: c.ChangeID, Prompt: prompt})
	if err != nil {
		return refuse("launch failed, and the change is left a draft: " + firstLine(err.Error()))
	}
	act.RunID = runID
	d.save(l)
	d.emit(runID, event.ChangeRestackDispatched, map[string]interface{}{
		"repo": c.Repo, "change_id": c.ChangeID, "handle": c.Handle, "version": c.Version,
		"head": c.Head, "target_tip": c.TargetTip, "report": c.Report,
		"attempt": e.Attempts, "max_attempts": MaxAttempts,
	})
	v.State, v.RunID, v.Attempts = StateDispatched, runID, e.Attempts
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

// emitStalled says klaus stopped short of resolving a change's conflict.
// It goes out under the change ID, which no run has.
func (d Deps) emitStalled(c Conflict, reason string) {
	d.emit(c.ChangeID, event.ChangeRestackStalled, map[string]interface{}{
		"repo": c.Repo, "change_id": c.ChangeID, "handle": c.Handle, "version": c.Version,
		"head": c.Head, "target_tip": c.TargetTip, "report": c.Report, "reason": reason,
	})
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

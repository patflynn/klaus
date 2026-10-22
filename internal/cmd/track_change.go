package cmd

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// changeRefPattern matches a valley change as klaus track and untrack take
// it: a handle or a full change ID (4 to 32 of the letters k to z a change ID
// is made of), optionally after "<project>:".
var changeRefPattern = regexp.MustCompile(`^(?:([^:\s]+):)?([k-z]{4,32})$`)

// parseChangeRef splits a valley change ref into its project ("" when it
// names none) and its handle or change ID. ok is false when ref isn't one.
func parseChangeRef(ref string) (project, id string, ok bool) {
	m := changeRefPattern.FindStringSubmatch(ref)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// valleyListings caches the valley's listing of each checkout klaus track
// asks about, so one command asks once per repo.
type valleyListings map[string]*valley.Changes

// trackChange tracks the valley change id names (a handle or a change ID) in
// project, a registered project name, owner/repo or URL with a local
// checkout, or "" for the repo of the current directory. It asks the valley
// about that checkout and saves a track record of the change: its ID, the
// valley's name for its repo, its handle, and its active version's number,
// title, branch and head. It returns the record, or nil when it saved none:
// the change is already on the dashboard, tracked or pushed by a run in
// states, or the valley doesn't list exactly one change id names.
func trackChange(ctx context.Context, proj, id string, store run.StateStore, states []*run.State, listings valleyListings) (*run.State, error) {
	dir := existingRepoDir(proj)
	switch {
	case dir == "" && proj == "":
		return nil, fmt.Errorf("no project for change %s: name it as <project>:%s, use --repo, or set a session target", id, id)
	case dir == "":
		return nil, fmt.Errorf("no checkout of %s on this machine: register it with klaus project add", proj)
	}
	l, ok := listings[dir]
	if !ok {
		var err error
		if l, err = valley.ListRepo(ctx, dir); err != nil {
			return nil, fmt.Errorf("asking the valley about %s: %w", dir, err)
		}
		listings[dir] = l
	}
	where := proj
	if where == "" {
		where = dir
	}
	type found struct {
		repo string
		ch   valley.Change
	}
	var matches []found
	for _, vr := range l.Repos {
		if vr.Err() != "" {
			continue
		}
		for _, ch := range vr.Changes {
			_, prefix, _ := strings.Cut(ch.Handle, ":") // a handle the valley qualifies as <repo>:<prefix>
			if refersTo(id, ch.ChangeID, ch.Handle, prefix) {
				matches = append(matches, found{vr.Repo, ch})
			}
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("the valley lists no change %s in %s", id, where)
	case 1:
	default:
		var lines []string
		for _, f := range matches {
			lines = append(lines, changeName(f.ch.ChangeID, f.ch.Handle)+" in "+f.repo)
		}
		return nil, ambiguous(id, where, lines)
	}
	repo, ch := matches[0].repo, matches[0].ch

	for _, s := range states {
		if s.Type != "session" && s.ChangeID == ch.ChangeID && integration.RepoName(s) == repo {
			fmt.Printf("Already tracking change %s (%s)\n", ch.Handle, repo)
			return nil, nil
		}
	}

	var active valley.Version
	for _, v := range ch.Versions {
		if v.N == ch.Active {
			active = v
		}
	}
	switch {
	case active.Landing == valley.Landed || active.Landing == valley.LandedRecorded:
		fmt.Fprintf(os.Stderr, "warning: change %s has landed\n", ch.Handle)
	case ch.State == valley.StateWithdrawn:
		fmt.Fprintf(os.Stderr, "warning: change %s is withdrawn\n", ch.Handle)
	}
	title := active.Title
	if title == "" {
		title = "change " + ch.Handle
	}

	target := repo
	if proj != "" {
		reg, _ := project.Load()
		target = project.NormalizeRepoName(proj, reg)
	}
	runID, err := run.GenID()
	if err != nil {
		return nil, err
	}
	st := &run.State{
		ID:         runID,
		Prompt:     title,
		Branch:     active.Branch,
		Type:       "track",
		TargetRepo: &target,
		CloneDir:   &dir,
		CreatedAt:  time.Now().Format(time.RFC3339),
		ChangeID:   ch.ChangeID,
		HeadSHA:    active.Head,
		RepoKey:    "valley:" + repo,
		Valley:     &run.Valley{Repo: repo, Handle: ch.Handle, Version: ch.Active, Title: active.Title},
	}
	if err := store.Save(st); err != nil {
		return nil, fmt.Errorf("saving state: %w", err)
	}
	fmt.Printf("Tracking change %s (%s) — %s\n", ch.Handle, repo, title)
	return st, nil
}

// trackedChanges returns the tracked valley changes among states that id (a
// handle or a change ID) names, in project when it isn't "": by the valley's
// name for the change's repo, or the name its project had when it was
// tracked.
func trackedChanges(states []*run.State, proj, id string) []*run.State {
	var out []*run.State
	for _, s := range states {
		if !integration.TracksChange(s) || !refersTo(id, s.ChangeID, s.Valley.Handle) {
			continue
		}
		if proj != "" && proj != s.Valley.Repo && proj != deref(s.TargetRepo) {
			reg, _ := project.Load()
			if project.NormalizeRepoName(proj, reg) != deref(s.TargetRepo) {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

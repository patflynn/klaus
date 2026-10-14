package cmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// changeTarget is the change a --change launch continues.
type changeTarget struct {
	ID     string       // the change ID
	Name   string       // how launch names it: its handle, else its ID
	Repo   string       // the change's repo, as integration.RepoName names it; "" when unknown
	Branch string       // the branch its work is on; "" when none is known
	Head   string       // that branch's last known head
	Runs   []*run.State // klaus's runs on the change, newest first
}

// changeSources is what resolveChange reads.
type changeSources struct {
	Runs []*run.State // every recorded run, from every session
	// Repo is the short name of the new run's target repo (targetName); ""
	// when unknown, and then runs in any repo count.
	Repo string
	// Valley lists the changes of the target repo, as `valley changes
	// --json` does, or returns errNoValley when no valley serves it. nil
	// means no valley. resolveChange calls it at most once.
	Valley func() (*valley.Changes, error)
}

// errNoValley is changeSources.Valley's answer when no valley serves the
// target repo.
var errNoValley = errors.New("no valley serves the repo")

// resolveChange finds the change ref names, for a launch that continues it.
// ref is a run ID, a full change ID or a valley handle:
//
//   - a run ID names that run's change;
//   - a change ID or handle names the change of the runs in the target repo
//     whose change ID it is, or starts with it, or whose recorded valley
//     handle it is;
//   - when no run matches, the valley's listing of the target repo names the
//     change whose ID or handle it is, or whose ID starts with it.
//
// A ref that names more than one change is an error listing them. A run is
// in the target repo when its integration.RepoName, or the short name of its
// own target repo, is the target's short name.
//
// The change's branch and head are its active version's when the valley
// lists one with a branch, else those the newest run on it pushed; its name
// is the valley's handle, else the newest recorded one, else its ID.
func resolveChange(ref string, src changeSources) (*changeTarget, error) {
	var (
		asked   bool
		listing *valley.Changes
		listErr = errNoValley
	)
	list := func() (*valley.Changes, error) {
		if !asked && src.Valley != nil {
			listing, listErr = src.Valley()
		}
		asked = true
		return listing, listErr
	}
	inRepo := func(s *run.State) bool {
		return s.Type != "session" && (src.Repo == "" || integration.RepoName(s) == src.Repo || targetName(s.TargetRepo) == src.Repo)
	}
	where := "this repo"
	if src.Repo != "" {
		where = src.Repo
	}

	var id string
	if s := findRun(src.Runs, ref); s != nil {
		if !run.ValidChangeID(s.ChangeID) {
			return nil, fmt.Errorf("run %s has no change ID (its state predates change IDs); --resume-from %s continues its conversation on a new branch", s.ID, s.ID)
		}
		if !inRepo(s) {
			other := integration.RepoName(s)
			if other == "" {
				other = "another repo"
			}
			return nil, fmt.Errorf("run %s is in %s, not %s; target its repo with --repo", s.ID, other, src.Repo)
		}
		id = s.ChangeID
	} else {
		named := make(map[string]*run.State) // change ID → its newest run that ref names
		handles := make(map[string]string)   // change ID → a handle recorded for it
		for _, s := range src.Runs {
			if !inRepo(s) || !run.ValidChangeID(s.ChangeID) || !refersTo(ref, s.ChangeID, recordedHandles(s)...) {
				continue
			}
			if prev, ok := named[s.ChangeID]; !ok || createdBefore(prev, s) {
				named[s.ChangeID] = s
			}
			if h := handleOf(s); h != "" {
				handles[s.ChangeID] = h
			}
		}
		switch len(named) {
		case 1:
			for k := range named {
				id = k
			}
		case 0:
			l, err := list()
			if err != nil {
				msg := fmt.Sprintf("no run on a change %q is recorded in %s", ref, where)
				if errors.Is(err, errNoValley) {
					return nil, errors.New(msg)
				}
				return nil, fmt.Errorf("%s, and the valley couldn't be asked: %w", msg, err)
			}
			found := make(map[string]valley.Change)
			for _, vr := range l.Repos {
				for _, ch := range vr.Changes {
					if vr.Err() == "" && refersTo(ref, ch.ChangeID, ch.Handle) {
						found[ch.ChangeID] = ch
					}
				}
			}
			if len(found) == 0 {
				return nil, fmt.Errorf("no change %q: no run in %s records it, and the valley doesn't list it", ref, where)
			}
			if len(found) > 1 {
				var lines []string
				for _, ch := range found {
					lines = append(lines, changeName(ch.ChangeID, ch.Handle))
				}
				return nil, ambiguous(ref, where, lines)
			}
			for k := range found {
				id = k
			}
		default:
			var lines []string
			for _, s := range named {
				line := changeName(s.ChangeID, handles[s.ChangeID]) + ", latest run " + s.ID
				if title := firstLine(runTitle(s)); title != "" {
					line += ": " + title
				}
				lines = append(lines, line)
			}
			return nil, ambiguous(ref, where, lines)
		}
	}

	t := &changeTarget{ID: id}
	for _, s := range src.Runs {
		if s.ChangeID == id && inRepo(s) {
			t.Runs = append(t.Runs, s)
		}
	}
	sort.SliceStable(t.Runs, func(a, b int) bool { return createdBefore(t.Runs[b], t.Runs[a]) })

	// The valley's view of the change's active version comes first: it may
	// have been marked ready elsewhere, after klaus's newest run.
	if l, err := list(); err == nil {
		if vr, ch, v, ok := activeVersionOf(l, id); ok {
			t.Repo, t.Name = vr, ch.Handle
			if v.Branch != "" {
				t.Branch, t.Head = v.Branch, v.Head
			}
		}
	}
	for _, s := range t.Runs {
		if t.Repo == "" {
			t.Repo = integration.RepoName(s)
		}
		if t.Name == "" {
			t.Name = handleOf(s)
		}
		if t.Branch == "" && s.PushedBranch != nil && *s.PushedBranch != "" {
			t.Branch = *s.PushedBranch
			if s.PushedSHA != nil {
				t.Head = *s.PushedSHA
			}
		}
	}
	if t.Name == "" {
		t.Name = id
	}
	return t, nil
}

// checkBranch reports an error unless the change's branch is on origin, as
// fetched into repoRoot: the worktree tracks origin's branch.
func (t *changeTarget) checkBranch(ctx context.Context, repoRoot string) error {
	if t.Branch == "" {
		return fmt.Errorf("change %s has no known branch or head: no run on it recorded a pushed branch, and no valley lists one", t.Name)
	}
	if !git.RefExists(ctx, repoRoot, "refs/remotes/origin/"+t.Branch) {
		head := t.Head
		if head == "" {
			head = "unknown"
		}
		return fmt.Errorf("change %s's branch %s is not on origin (last known head %s)", t.Name, t.Branch, head)
	}
	return nil
}

// checkContinuation rejects launch flags that pick a branch two ways: --pr
// works on the PR's branch and --change on the change's, and --branch names
// a new one.
func checkContinuation(pr, change string, branchSet bool) error {
	switch {
	case pr != "" && change != "":
		return fmt.Errorf("--pr and --change are mutually exclusive: --pr continues a PR, --change a change without one")
	case branchSet && pr != "":
		return fmt.Errorf("--branch and --pr are mutually exclusive: --pr works on the PR's branch")
	case branchSet && change != "":
		return fmt.Errorf("--branch and --change are mutually exclusive: --change works on the change's branch")
	}
	return nil
}

// targetName is the short name of a target repo, as integration.RepoName
// names a run by it: its last path component, without ".git".
func targetName(target *string) string {
	return integration.RepoName(&run.State{TargetRepo: target})
}

// findRun returns the run with ID id, or nil.
func findRun(states []*run.State, id string) *run.State {
	for _, s := range states {
		if s.ID == id && s.Type != "session" {
			return s
		}
	}
	return nil
}

// refersTo reports whether ref names the change with ID changeID and the
// given handles: it is the ID, or starts it, or is one of the handles.
func refersTo(ref, changeID string, handles ...string) bool {
	if strings.HasPrefix(changeID, ref) {
		return true
	}
	for _, h := range handles {
		if h != "" && h == ref {
			return true
		}
	}
	return false
}

// recordedHandles are the valley handles klaus recorded for a run's change:
// finalize's `valley ready` and launch's `valley unready`.
func recordedHandles(s *run.State) []string {
	var hs []string
	if s.Valley != nil {
		hs = append(hs, s.Valley.Handle)
	}
	if s.Unready != nil {
		hs = append(hs, s.Unready.Handle)
	}
	return hs
}

// handleOf is the handle recorded for a run's change, or "".
func handleOf(s *run.State) string {
	for _, h := range recordedHandles(s) {
		if h != "" {
			return h
		}
	}
	return ""
}

// runTitle is what a run is about: the subject of the head it pushed, else
// its prompt.
func runTitle(s *run.State) string {
	if s.Valley != nil && s.Valley.Title != "" {
		return s.Valley.Title
	}
	if s.PushedSubject != "" {
		return s.PushedSubject
	}
	return s.Prompt
}

// firstLine is s up to its first newline.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// changeName names a change by its ID and, when known, its handle.
func changeName(id, handle string) string {
	if handle == "" {
		return id
	}
	return id + " (" + handle + ")"
}

// ambiguous is the error for a ref that names several changes, one per line.
func ambiguous(ref, where string, lines []string) error {
	sort.Strings(lines)
	return fmt.Errorf("%q names %d changes in %s:\n  %s\nname one by its change ID or a run ID", ref, len(lines), where, strings.Join(lines, "\n  "))
}

// activeVersionOf finds change id in a valley listing and returns the repo
// it is listed in, the change and its active version. A repo the valley
// couldn't read is skipped; where several records hold the ID, one that
// isn't withdrawn wins.
func activeVersionOf(l *valley.Changes, id string) (repo string, ch valley.Change, v valley.Version, ok bool) {
	for _, vr := range l.Repos {
		if vr.Err() != "" {
			continue
		}
		for _, c := range vr.Changes {
			if c.ChangeID != id || (ok && ch.State != valley.StateWithdrawn) {
				continue
			}
			for _, cv := range c.Versions {
				if cv.N == c.Active {
					repo, ch, v, ok = vr.Repo, c, cv, true
				}
			}
		}
	}
	return
}

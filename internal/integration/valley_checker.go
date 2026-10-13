package integration

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// ValleyChecker asks a valley where pushed runs' changes stand, and defers to
// a GitChecker for the runs the valley doesn't place.
//
// A check runs `valley changes --all --json` once, for every repo the valley
// serves, and only when finalize found that some run's repo delivers to a
// valley (run.State.Valley). It places a run by repo, change ID and version
// head: the run's version is the one whose head is the run's pushed SHA. If
// valley fails or prints a schema klaus doesn't know, the check is logged at
// debug level and every run is judged by git.
type ValleyChecker struct {
	git    *GitChecker
	logger *slog.Logger
}

// NewValleyChecker returns a ValleyChecker that logs failed checks at debug
// level to logger (slog.Default when nil).
func NewValleyChecker(logger *slog.Logger) *ValleyChecker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ValleyChecker{git: NewGitChecker(logger), logger: logger}
}

// Check returns a result for every run that Applies.
func (c *ValleyChecker) Check(ctx context.Context, runs []*run.State) map[string]Result {
	dir, ok := valleyDir(runs)
	if !ok {
		return c.git.Check(ctx, runs)
	}
	changes, err := valley.List(ctx, dir)
	if err != nil {
		c.logger.Debug("valley check failed; judging runs by git", "dir", dir, "err", err)
		return c.git.Check(ctx, runs)
	}
	out := make(map[string]Result)
	var rest []*run.State
	for _, s := range runs {
		if r, ok := place(changes, s); ok {
			out[s.ID] = r
		} else {
			rest = append(rest, s)
		}
	}
	for id, r := range c.git.Check(ctx, rest) {
		out[id] = r
	}
	return out
}

// valleyDir picks where to run valley: the clone of a run whose repo
// delivers to a valley, or the current directory when no such clone is
// there. ok is false when no run's repo delivers to a valley.
func valleyDir(runs []*run.State) (dir string, ok bool) {
	for _, s := range runs {
		if !Applies(s) || s.Valley == nil {
			continue
		}
		ok = true
		if s.CloneDir != nil && *s.CloneDir != "" {
			if fi, err := os.Stat(*s.CloneDir); err == nil && fi.IsDir() {
				return *s.CloneDir, true
			}
		}
	}
	return "", ok
}

// place finds a run's version among the valley's changes: in the run's repo,
// a change with the run's change ID and a version whose head is the run's
// pushed SHA. When several match, the one that is its change's active version
// wins. ok is false when none does.
func place(changes *valley.Changes, s *run.State) (Result, bool) {
	if !Applies(s) || s.ChangeID == "" {
		return Result{}, false
	}
	repo := valleyRepoName(s)
	var found Result
	ok := false
	for _, vr := range changes.Repos {
		if repo != "" && vr.Repo != repo {
			continue
		}
		for _, ch := range vr.Changes {
			if ch.ChangeID != s.ChangeID {
				continue
			}
			v, match := versionAt(ch, *s.PushedSHA)
			if !match {
				continue
			}
			r := Result{
				Verdict: verdictOf(ch, v),
				Change: &Change{Repo: vr.Repo, ChangeID: ch.ChangeID, Handle: ch.Handle, Title: v.Title,
					Version: v.N, Head: v.Head},
			}
			if v.N == ch.Active {
				return r, true
			}
			if !ok {
				found, ok = r, true
			}
		}
	}
	return found, ok
}

// valleyRepoName is the valley's name for a run's repo, as finalize's
// `valley ready` reported it, or else the short name of the run's target
// repo. "" means unknown: any repo matches.
func valleyRepoName(s *run.State) string {
	if s.Valley != nil && s.Valley.Repo != "" {
		return s.Valley.Repo
	}
	if s.TargetRepo == nil {
		return ""
	}
	name := *s.TargetRepo
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".git")
}

// versionAt returns the version of ch whose head is head: the active one if
// that head was marked ready more than once, else the latest.
func versionAt(ch valley.Change, head string) (valley.Version, bool) {
	var found valley.Version
	ok := false
	for _, v := range ch.Versions {
		if v.Head != head {
			continue
		}
		if v.N == ch.Active {
			return v, true
		}
		if !ok || v.N > found.N {
			found, ok = v, true
		}
	}
	return found, ok
}

// verdictOf reads the valley's record of version v of change ch. Landing
// evidence comes first: a landed version is Integrated even after a later
// version took its place.
func verdictOf(ch valley.Change, v valley.Version) Verdict {
	switch {
	case v.Landing == valley.Landed || v.Landing == valley.LandedRecorded:
		return Integrated
	case ch.State == valley.StateWithdrawn:
		return Withdrawn
	case v.N != ch.Active:
		return Superseded
	case v.Landing == valley.Uncertain:
		return Uncertain
	case v.Outcome != nil:
		switch strings.ToLower(v.Outcome.Decision) {
		case "stale":
			return Stale
		case "rejected":
			return Rejected
		case "failed":
			return Failed
		}
		return Uncertain
	case ch.State != valley.StateReady || v.Landing != valley.NotLanded:
		// A state or landing value from a later valley.
		return Uncertain
	case v.Request != nil:
		return Approved
	default:
		return Ready
	}
}

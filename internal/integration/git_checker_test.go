package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// fixture is a bare origin with two clones: the run's clone (the agent's
// CloneDir, which the checker reads) and the operator's clone, which lands
// and deletes branches the way the operator or an integrator would.
type fixture struct {
	t      *testing.T
	origin string
	clone  string
	ops    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	isolateGit(t)
	dir := t.TempDir()
	f := &fixture{
		t:      t,
		origin: filepath.Join(dir, "origin.git"),
		clone:  filepath.Join(dir, "clone"),
		ops:    filepath.Join(dir, "ops"),
	}
	git(t, dir, "init", "-q", "--bare", "-b", "main", f.origin)
	git(t, dir, "clone", "-q", f.origin, f.ops)
	writeFile(t, f.ops, "README", "hello\n")
	git(t, f.ops, "add", "README")
	git(t, f.ops, "commit", "-q", "-m", "init")
	git(t, f.ops, "push", "-q", "origin", "HEAD:main")
	git(t, dir, "clone", "-q", f.origin, f.clone)
	return f
}

// isolateGit keeps the user's git config out of the test and gives commits
// an identity.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.com")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// agentPush does what a direct-push agent does: commits each file on a new
// branch cut from origin's main, pushes the branch, and (like _finalize)
// deletes the local branch. It returns the run as finalize records it.
func (f *fixture) agentPush(id, branch string, files ...string) *run.State {
	f.t.Helper()
	git(f.t, f.clone, "fetch", "-q", "origin")
	git(f.t, f.clone, "checkout", "-q", "-b", branch, "origin/main")
	for _, name := range files {
		writeFile(f.t, f.clone, name, name+" from "+branch+"\n")
		git(f.t, f.clone, "add", name)
		git(f.t, f.clone, "commit", "-q", "-m", "add "+name)
	}
	git(f.t, f.clone, "push", "-q", "origin", branch)
	sha := git(f.t, f.clone, "rev-parse", "HEAD")
	git(f.t, f.clone, "checkout", "-q", "--detach")
	git(f.t, f.clone, "branch", "-q", "-D", branch)
	clone := f.clone
	return &run.State{
		ID:           id,
		PushedBranch: &branch,
		PushedSHA:    &sha,
		CloneDir:     &clone,
		CreatedAt:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}
}

// ops runs git in the operator's clone after fetching origin.
func (f *fixture) opsFetch() {
	f.t.Helper()
	git(f.t, f.ops, "fetch", "-q", "--prune", "origin")
}

// advanceMain lands an unrelated commit on main.
func (f *fixture) advanceMain(name string) {
	f.t.Helper()
	f.opsFetch()
	git(f.t, f.ops, "checkout", "-q", "-B", "main", "origin/main")
	writeFile(f.t, f.ops, name, "unrelated\n")
	git(f.t, f.ops, "add", name)
	git(f.t, f.ops, "commit", "-q", "-m", "unrelated "+name)
	git(f.t, f.ops, "push", "-q", "origin", "main")
}

// advanceMainAt lands an unrelated commit on main, committed at when.
func (f *fixture) advanceMainAt(name string, when time.Time) {
	f.t.Helper()
	f.t.Setenv("GIT_COMMITTER_DATE", fmt.Sprintf("@%d +0000", when.Unix()))
	defer os.Unsetenv("GIT_COMMITTER_DATE")
	f.advanceMain(name)
}

// fastForward moves main to sha.
func (f *fixture) fastForward(sha string) {
	f.t.Helper()
	f.opsFetch()
	git(f.t, f.ops, "push", "-q", "origin", sha+":refs/heads/main")
}

// integrate lands branch the way the valley's integrator does when it can't
// fast-forward: one new commit whose only parent is main's tip and whose tree
// is the merge-tree of main and the branch.
func (f *fixture) integrate(branch string) {
	f.t.Helper()
	f.opsFetch()
	main := git(f.t, f.ops, "rev-parse", "origin/main")
	tip := git(f.t, f.ops, "rev-parse", "origin/"+branch)
	tree := git(f.t, f.ops, "merge-tree", "--write-tree", main, tip)
	commit := git(f.t, f.ops, "commit-tree", tree, "-p", main, "-m", "land "+branch)
	git(f.t, f.ops, "push", "-q", "origin", commit+":refs/heads/main")
}

// rebaseAndForcePush rebases branch onto main and force-pushes it, like the
// operator's [b]ase in valley review. It returns the new tip.
func (f *fixture) rebaseAndForcePush(branch string) string {
	f.t.Helper()
	f.opsFetch()
	git(f.t, f.ops, "checkout", "-q", "-B", branch, "origin/"+branch)
	git(f.t, f.ops, "rebase", "-q", "origin/main")
	git(f.t, f.ops, "push", "-q", "--force", "origin", branch)
	tip := git(f.t, f.ops, "rev-parse", "HEAD")
	git(f.t, f.ops, "checkout", "-q", "--detach")
	return tip
}

func (f *fixture) deleteBranch(branch string) {
	f.t.Helper()
	git(f.t, f.ops, "push", "-q", "origin", "--delete", branch)
}

func checkOne(t *testing.T, c Checker, s *run.State) Verdict {
	t.Helper()
	got := c.Check(context.Background(), []*run.State{s})
	r, ok := got[s.ID]
	if !ok {
		t.Fatalf("no verdict for %s: %v", s.ID, got)
	}
	return r.Verdict
}

func TestGitCheckerFastForwardLanding(t *testing.T) {
	f := newFixture(t)
	s := f.agentPush("r1", "agent/ff", "a.txt", "b.txt")
	f.fastForward(*s.PushedSHA)

	if v := checkOne(t, NewGitChecker(nil), s); v != Integrated {
		t.Errorf("verdict = %v, want integrated", v)
	}
}

func TestGitCheckerRebasedThenFastForwarded(t *testing.T) {
	// The operator rebases the branch onto main and force-pushes it, then it
	// fast-forwards: only the current tip is an ancestor of main.
	f := newFixture(t)
	s := f.agentPush("r1", "agent/rebased", "a.txt")
	c := NewGitChecker(nil)
	if v := checkOne(t, c, s); v != Awaiting {
		t.Fatalf("before landing: verdict = %v, want awaiting", v)
	}

	f.advanceMain("other.txt")
	tip := f.rebaseAndForcePush("agent/rebased")
	if v := checkOne(t, c, s); v != Awaiting {
		t.Fatalf("after rebase: verdict = %v, want awaiting", v)
	}

	f.fastForward(tip)
	if anc := exec.Command("git", "-C", f.ops, "merge-base", "--is-ancestor", *s.PushedSHA, tip).Run(); anc == nil {
		t.Fatal("test setup: the pushed SHA should not be an ancestor of the rebased tip")
	}
	if v := checkOne(t, c, s); v != Integrated {
		t.Errorf("after fast-forward: verdict = %v, want integrated", v)
	}
}

func TestGitCheckerIntegratorLanding(t *testing.T) {
	// A multi-commit branch lands as one new commit on top of a main that
	// moved, on unrelated files, after the branch was cut.
	f := newFixture(t)
	s := f.agentPush("r1", "agent/multi", "a.txt", "b.txt", "c.txt")
	f.advanceMain("unrelated-1.txt")
	f.advanceMain("unrelated-2.txt")
	f.integrate("agent/multi")

	c := NewGitChecker(nil)
	if v := checkOne(t, c, s); v != IntegratedContent {
		t.Errorf("verdict = %v, want integrated by content", v)
	}

	// Landed, then the branch is deleted: still integrated, never gone.
	f.deleteBranch("agent/multi")
	if v := checkOne(t, c, s); v != IntegratedContent {
		t.Errorf("after delete: verdict = %v, want integrated by content", v)
	}
}

func TestGitCheckerDeletedAfterFastForward(t *testing.T) {
	f := newFixture(t)
	s := f.agentPush("r1", "agent/ff-del", "a.txt")
	f.fastForward(*s.PushedSHA)
	f.deleteBranch("agent/ff-del")

	if v := checkOne(t, NewGitChecker(nil), s); v != Integrated {
		t.Errorf("verdict = %v, want integrated", v)
	}
}

func TestGitCheckerDeletedWithoutLanding(t *testing.T) {
	f := newFixture(t)
	s := f.agentPush("r1", "agent/rejected", "a.txt")
	f.advanceMain("other.txt")
	f.deleteBranch("agent/rejected")

	if v := checkOne(t, NewGitChecker(nil), s); v != Gone {
		t.Errorf("verdict = %v, want gone", v)
	}
}

func TestGitCheckerDeletedAndPrunedIsUnknown(t *testing.T) {
	// The branch was deleted without landing and the clone no longer has the
	// pushed commit, so nothing rules out that it landed: unknown, not gone.
	f := newFixture(t)
	s := f.agentPush("r1", "agent/pruned", "a.txt")
	f.advanceMain("other.txt")
	f.deleteBranch("agent/pruned")
	git(t, f.clone, "fetch", "-q", "--prune", "origin")
	git(t, f.clone, "checkout", "-q", "--detach", "origin/main")
	git(t, f.clone, "reflog", "expire", "--expire=now", "--all")
	git(t, f.clone, "gc", "-q", "--prune=now")
	if exec.Command("git", "-C", f.clone, "cat-file", "-e", *s.PushedSHA).Run() == nil {
		t.Fatal("test setup: the pushed commit should be pruned from the clone")
	}

	c := NewGitChecker(nil)
	if v := checkOne(t, c, s); v != Unknown {
		t.Fatalf("verdict = %v, want unknown", v)
	}

	// The commit comes back while origin's tips stay put: the unknown wasn't
	// cached, so the next check has the evidence and says gone.
	git(t, f.ops, "push", "-q", f.clone, *s.PushedSHA+":refs/heads/restored")
	if v := checkOne(t, c, s); v != Gone {
		t.Errorf("after the commit is back: verdict = %v, want gone", v)
	}
}

func TestGitCheckerTruncatedScanIsNotGone(t *testing.T) {
	// Two deleted branches that never landed. The walk of main runs back to
	// the older run's creation but stops at the cap first: it covers the
	// newer run, which is gone, but not the older one, whose landing it might
	// have missed.
	now := time.Now()
	f := newFixture(t)
	old := f.agentPush("old", "agent/old", "a.txt")
	newer := f.agentPush("newer", "agent/newer", "b.txt")
	old.CreatedAt = now.Add(-time.Hour).UTC().Format(time.RFC3339)
	newer.CreatedAt = now.Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	f.advanceMainAt("o1.txt", now.Add(-30*time.Minute))
	f.advanceMainAt("o2.txt", now.Add(-20*time.Minute))
	f.advanceMain("o3.txt")
	f.deleteBranch("agent/old")
	f.deleteBranch("agent/newer")
	runs := []*run.State{old, newer}

	c := NewGitChecker(nil)
	c.scanLimit = 2 // o3 and o2
	got := c.Check(context.Background(), runs)
	if got["old"].Verdict != Unknown {
		t.Errorf("old: verdict = %v, want unknown", got["old"].Verdict)
	}
	if got["newer"].Verdict != Gone {
		t.Errorf("newer: verdict = %v, want gone", got["newer"].Verdict)
	}

	got = NewGitChecker(nil).Check(context.Background(), runs)
	for _, s := range runs {
		if got[s.ID].Verdict != Gone {
			t.Errorf("%s without a cap: verdict = %v, want gone", s.ID, got[s.ID].Verdict)
		}
	}
}

func TestGitCheckerOneSnapshotForManyRuns(t *testing.T) {
	// Runs in the same clone share one check: one waiting, one landed by
	// fast-forward, one landed by the integrator, one deleted.
	f := newFixture(t)
	waiting := f.agentPush("waiting", "agent/waiting", "w.txt")
	ff := f.agentPush("ff", "agent/ff", "f.txt")
	content := f.agentPush("content", "agent/content", "c1.txt", "c2.txt")
	gone := f.agentPush("gone", "agent/gone", "g.txt")
	f.fastForward(*ff.PushedSHA)
	f.advanceMain("other.txt")
	f.integrate("agent/content")
	f.deleteBranch("agent/gone")

	got := NewGitChecker(nil).Check(context.Background(), []*run.State{waiting, ff, content, gone})
	want := map[string]Verdict{"waiting": Awaiting, "ff": Integrated, "content": IntegratedContent, "gone": Gone}
	for id, w := range want {
		if got[id].Verdict != w {
			t.Errorf("%s: verdict = %v, want %v", id, got[id].Verdict, w)
		}
	}
}

func TestGitCheckerUnreachableOriginIsUnknown(t *testing.T) {
	// A bad origin URL can't be listed: unknown, never gone, even though the
	// branch is nowhere to be seen.
	f := newFixture(t)
	s := f.agentPush("r1", "agent/x", "a.txt")
	git(t, f.clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "no-such-origin.git"))

	c := NewGitChecker(nil)
	if v := checkOne(t, c, s); v != Unknown {
		t.Fatalf("verdict = %v, want unknown", v)
	}

	// Once origin is fixed, the next check gives a real verdict.
	git(t, f.clone, "remote", "set-url", "origin", f.origin)
	if v := checkOne(t, c, s); v != Awaiting {
		t.Errorf("after fixing origin: verdict = %v, want awaiting", v)
	}
}

func TestGitCheckerWithoutCloneIsUnknown(t *testing.T) {
	isolateGit(t)
	branch, sha := "agent/x", "0123456789abcdef0123456789abcdef01234567"
	noClone := &run.State{ID: "no-clone", PushedBranch: &branch, PushedSHA: &sha}
	missing := filepath.Join(t.TempDir(), "gone")
	missingClone := &run.State{ID: "missing-clone", PushedBranch: &branch, PushedSHA: &sha, CloneDir: &missing}
	notPushed := &run.State{ID: "not-pushed"}

	got := NewGitChecker(nil).Check(context.Background(), []*run.State{noClone, missingClone, notPushed})
	if r, ok := got["no-clone"]; !ok || r.Verdict != Unknown {
		t.Errorf("no clone: verdict = %v (present %v), want unknown", r.Verdict, ok)
	}
	if r, ok := got["missing-clone"]; !ok || r.Verdict != Unknown {
		t.Errorf("missing clone: verdict = %v (present %v), want unknown", r.Verdict, ok)
	}
	if _, ok := got["not-pushed"]; ok {
		t.Error("a run that pushed nothing should get no verdict")
	}
}

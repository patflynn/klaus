package projectsync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/project"
)

// runGit runs a git command from dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// makeUpstream creates a bare origin repo with one initial commit and returns
// its path.
func makeUpstream(t *testing.T) string {
	t.Helper()
	seed := t.TempDir()
	runGit(t, ".", "init", "--initial-branch=main", seed)
	runGit(t, seed, "config", "user.email", "test@test.com")
	runGit(t, seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "initial")

	bare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "clone", "--bare", seed, bare).CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v\n%s", err, out)
	}
	return bare
}

// cloneFrom clones bare into a fresh directory and configures git user, returning the path.
func cloneFrom(t *testing.T, bare string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", bare, dir).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	return dir
}

// advanceOrigin pushes one new commit to the bare origin repo by doing the
// commit in a temporary staging clone.
func advanceOrigin(t *testing.T, bare string) {
	t.Helper()
	staging := cloneFrom(t, bare)
	fname := filepath.Join(staging, "advance.txt")
	if err := os.WriteFile(fname, []byte("advance"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, staging, "add", "advance.txt")
	runGit(t, staging, "commit", "-m", "advance origin")
	runGit(t, staging, "push", "origin", "main")
}

func makeRegistry(t *testing.T, projects map[string]string) *project.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "projects.json")
	reg := &project.Registry{
		ProjectsDir: t.TempDir(),
		Projects:    projects,
	}
	if err := reg.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := project.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func findResult(results []SyncResult, name string) *SyncResult {
	for i := range results {
		if results[i].Name == name {
			return &results[i]
		}
	}
	return nil
}

func TestSync_CleanAndBehindGetsPulled(t *testing.T) {
	bare := makeUpstream(t)
	clone := cloneFrom(t, bare)
	advanceOrigin(t, bare)

	reg := makeRegistry(t, map[string]string{"demo": clone})
	results := Sync(context.Background(), reg, git.NewExecClient())

	r := findResult(results, "demo")
	if r == nil {
		t.Fatalf("no result for demo; got %+v", results)
	}
	if r.Status != StatusPulled {
		t.Errorf("Status = %q, want %q (detail=%q)", r.Status, StatusPulled, r.Detail)
	}
	if r.Branch != "main" {
		t.Errorf("Branch = %q, want main", r.Branch)
	}
	if _, err := os.Stat(filepath.Join(clone, "advance.txt")); err != nil {
		t.Errorf("advance.txt should exist after fast-forward: %v", err)
	}
}

func TestSync_AlreadyUpToDate(t *testing.T) {
	bare := makeUpstream(t)
	clone := cloneFrom(t, bare)

	reg := makeRegistry(t, map[string]string{"demo": clone})
	results := Sync(context.Background(), reg, git.NewExecClient())
	r := findResult(results, "demo")
	if r == nil || r.Status != StatusUpToDate {
		t.Errorf("expected up-to-date, got %+v", r)
	}
}

func TestSync_DirtyTreeIsSkipped(t *testing.T) {
	bare := makeUpstream(t)
	clone := cloneFrom(t, bare)
	advanceOrigin(t, bare)

	// Dirty the working tree — untracked file is enough
	if err := os.WriteFile(filepath.Join(clone, "local.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := makeRegistry(t, map[string]string{"demo": clone})
	results := Sync(context.Background(), reg, git.NewExecClient())
	r := findResult(results, "demo")
	if r == nil || r.Status != StatusSkipped {
		t.Fatalf("expected skipped, got %+v", r)
	}
	if !strings.Contains(r.Detail, "dirty") {
		t.Errorf("Detail should mention dirty, got %q", r.Detail)
	}
	// Must NOT have fast-forwarded
	if _, err := os.Stat(filepath.Join(clone, "advance.txt")); !os.IsNotExist(err) {
		t.Error("sync should not touch a dirty clone")
	}
}

func TestSync_DivergedIsSkipped(t *testing.T) {
	bare := makeUpstream(t)
	clone := cloneFrom(t, bare)
	advanceOrigin(t, bare)

	// Commit a different change locally so we diverge from origin/main.
	if err := os.WriteFile(filepath.Join(clone, "local.txt"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", "local.txt")
	runGit(t, clone, "commit", "-m", "local change")

	reg := makeRegistry(t, map[string]string{"demo": clone})
	results := Sync(context.Background(), reg, git.NewExecClient())
	r := findResult(results, "demo")
	if r == nil {
		t.Fatalf("no result")
	}
	if r.Status != StatusSkipped {
		t.Errorf("Status = %q, want skipped (detail=%q)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "fast-forward") {
		t.Errorf("Detail should mention fast-forward, got %q", r.Detail)
	}
}

func TestSync_NoUpstreamIsSkipped(t *testing.T) {
	bare := makeUpstream(t)
	clone := cloneFrom(t, bare)

	// Check out a new branch with no upstream
	runGit(t, clone, "checkout", "-b", "feature")

	reg := makeRegistry(t, map[string]string{"demo": clone})
	results := Sync(context.Background(), reg, git.NewExecClient())
	r := findResult(results, "demo")
	if r == nil || r.Status != StatusSkipped {
		t.Fatalf("expected skipped, got %+v", r)
	}
	if !strings.Contains(r.Detail, "upstream") {
		t.Errorf("Detail should mention upstream, got %q", r.Detail)
	}
}

func TestSync_DetachedHeadIsSkipped(t *testing.T) {
	bare := makeUpstream(t)
	clone := cloneFrom(t, bare)
	runGit(t, clone, "checkout", "--detach")

	reg := makeRegistry(t, map[string]string{"demo": clone})
	results := Sync(context.Background(), reg, git.NewExecClient())
	r := findResult(results, "demo")
	if r == nil || r.Status != StatusSkipped {
		t.Fatalf("expected skipped, got %+v", r)
	}
	if !strings.Contains(r.Detail, "detached") {
		t.Errorf("Detail should mention detached, got %q", r.Detail)
	}
}

func TestSync_FetchErrorReported(t *testing.T) {
	// Point the clone at a non-existent origin so fetch fails.
	bare := makeUpstream(t)
	clone := cloneFrom(t, bare)
	bad := filepath.Join(t.TempDir(), "nonexistent.git")
	runGit(t, clone, "remote", "set-url", "origin", bad)

	reg := makeRegistry(t, map[string]string{"demo": clone})
	results := Sync(context.Background(), reg, git.NewExecClient())
	r := findResult(results, "demo")
	if r == nil || r.Status != StatusError {
		t.Fatalf("expected error, got %+v", r)
	}
}

func TestSync_ExcludePathSkipsMatchingProject(t *testing.T) {
	bare := makeUpstream(t)
	a := cloneFrom(t, bare)
	b := cloneFrom(t, bare)
	advanceOrigin(t, bare)

	// Exclude `a` — it should not appear in results at all, and must not be
	// fast-forwarded. `b` should still be synced normally.
	reg := makeRegistry(t, map[string]string{"alpha": a, "bravo": b})
	results := Sync(context.Background(), reg, git.NewExecClient(), a)

	if findResult(results, "alpha") != nil {
		t.Errorf("alpha should have been excluded, got %+v", results)
	}
	if r := findResult(results, "bravo"); r == nil || r.Status != StatusPulled {
		t.Errorf("bravo should be pulled, got %+v", r)
	}
	if _, err := os.Stat(filepath.Join(a, "advance.txt")); !os.IsNotExist(err) {
		t.Errorf("excluded clone %q must not be fast-forwarded", a)
	}
}

func TestSync_BoundedConcurrency(t *testing.T) {
	// Register more than MaxConcurrency projects. All should complete despite
	// the cap. This is a smoke test — the main guarantee we care about is that
	// bounded concurrency doesn't drop any project from the results.
	bare := makeUpstream(t)
	projects := make(map[string]string, MaxConcurrency+2)
	for i := 0; i < MaxConcurrency+2; i++ {
		projects[fmt.Sprintf("p%02d", i)] = cloneFrom(t, bare)
	}
	reg := makeRegistry(t, projects)
	results := Sync(context.Background(), reg, git.NewExecClient())
	if len(results) != len(projects) {
		t.Fatalf("got %d results, want %d", len(results), len(projects))
	}
	for _, r := range results {
		if r.Status != StatusUpToDate {
			t.Errorf("%s: want up-to-date, got %q (%s)", r.Name, r.Status, r.Detail)
		}
	}
}

func TestSync_SortsByName(t *testing.T) {
	bare := makeUpstream(t)
	a := cloneFrom(t, bare)
	b := cloneFrom(t, bare)
	c := cloneFrom(t, bare)

	reg := makeRegistry(t, map[string]string{
		"zebra": a,
		"alpha": b,
		"mango": c,
	})
	results := Sync(context.Background(), reg, git.NewExecClient())
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	want := []string{"alpha", "mango", "zebra"}
	for i, w := range want {
		if results[i].Name != w {
			t.Errorf("results[%d].Name = %q, want %q", i, results[i].Name, w)
		}
	}
}

package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/consult"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/jobs"
)

func snapshotGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s, %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

// commitToOrigin commits main.txt holding content on main in the seed clone
// and pushes it to origin, returning the commit.
func commitToOrigin(t *testing.T, seed, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(seed, "main.txt"), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotGit(t, seed, "add", "main.txt")
	snapshotGit(t, seed, "commit", "-qm", content)
	snapshotGit(t, seed, "push", "-q", "origin", "HEAD:main")
	return snapshotGit(t, seed, "rev-parse", "HEAD")
}

// TestConsultRepoReadsOriginSnapshot runs the real klaus against a bare
// origin and a registered clone checked out on a stale branch, with a stub
// backend that records where it ran and what it read.
func TestConsultRepoReadsOriginSnapshot(t *testing.T) {
	bin := klausBinary(t)
	e := newConsultEnv(t, bin, "session-consult-snapshot")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin, seed, clone := filepath.Join(root, "origin.git"), filepath.Join(root, "seed"), filepath.Join(root, "clone")
	snapshotGit(t, root, "init", "-q", "--bare", origin)
	snapshotGit(t, root, "clone", "-q", origin, seed)
	c1 := commitToOrigin(t, seed, "v1")
	snapshotGit(t, root, "clone", "-q", origin, clone)
	snapshotGit(t, clone, "checkout", "-qb", "stale")
	if err := os.WriteFile(filepath.Join(clone, "stale.txt"), []byte("old agent work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotGit(t, clone, "add", "stale.txt")
	snapshotGit(t, clone, "commit", "-qm", "stale")
	reg, _ := json.Marshal(map[string]interface{}{"projects": map[string]string{"widget": clone}})
	if err := os.MkdirAll(filepath.Join(e.home, ".klaus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.home, ".klaus", "projects.json"), reg, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := t.TempDir()
	e.stub(t, "codex", `rec=`+rec+`
pwd -P > "$rec/dir"
ls -A > "$rec/files"
cat main.txt > "$rec/main.txt"
cat > "$rec/prompt"
echo an answer
echo 'session id: snapshot-thread' >&2
`)
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(rec, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
	}
	// turn runs a consult and checks the backend read origin's main at commit
	// in dir, with a prompt headed by the workspace line.
	turn := func(t *testing.T, commit, content, dir string, args ...string) string {
		t.Helper()
		out, err := e.run(t, args...)
		if err != nil {
			t.Fatalf("consult %v: %s, %v", args, out, err)
		}
		if got := read("dir"); got != dir {
			t.Errorf("backend ran in %s, want %s", got, dir)
		}
		if got := read("main.txt"); got != content {
			t.Errorf("main.txt = %q, want %q", got, content)
		}
		if strings.Contains(read("files"), "stale.txt") {
			t.Errorf("backend saw the clone's stale branch: %s", read("files"))
		}
		want := "Workspace: widget at origin/main " + commit
		if first, _, _ := strings.Cut(read("prompt"), "\n"); first != want {
			t.Errorf("prompt starts %q, want %q", first, want)
		}
		return out
	}
	store := consult.NewStore(e.sessionDir())
	snap, err := store.WorkspaceDir("design")
	if err != nil {
		t.Fatal(err)
	}

	turn(t, c1, "v1", snap, "--backend", "codex", "--thread", "design", "--repo", "widget", "first question")
	saved, err := store.Load("design")
	if err != nil || saved.Repo != "widget" || saved.Commit != c1 || saved.Dir != snap {
		t.Fatalf("saved thread: %+v, %v", saved, err)
	}
	var started event.Event
	for _, evt := range e.events(t) {
		if evt.Type == event.ConsultStarted {
			started = evt
		}
	}
	if started.Data["commit"] != c1 || started.Data["repo"] != "widget" {
		t.Errorf("consult:started data = %+v", started.Data)
	}
	job, err := jobs.NewStore(e.sessionDir()).Load(started.RunID)
	if err != nil || job.Commit != c1 || job.Repo != "widget" {
		t.Errorf("job record: %+v, %v", job, err)
	}
	if branch := snapshotGit(t, clone, "branch", "--show-current"); branch != "stale" {
		t.Errorf("clone moved to %s", branch)
	}

	c2 := commitToOrigin(t, seed, "v2")
	turn(t, c2, "v2", snap, "--thread", "design", "second question")
	if log, _ := os.ReadFile(filepath.Join(store.Dir, "design.log")); !strings.Contains(string(log), `"repo_revision":"`+c2+`"`) {
		t.Errorf("turn 2 doesn't record %s: %s", c2, log)
	}

	// A snapshot deleted behind git's back is pruned and made again.
	if err := os.RemoveAll(snap); err != nil {
		t.Fatal(err)
	}
	turn(t, c2, "v2", snap, "--thread", "design", "--repo", "widget", "third question")

	t.Run("dir reads as it stands", func(t *testing.T) {
		if out, err := e.run(t, "--backend", "codex", "--dir", clone, "question"); err != nil {
			t.Fatalf("consult: %s, %v", out, err)
		}
		if read("dir") != clone || !strings.Contains(read("files"), "stale.txt") || strings.HasPrefix(read("prompt"), "Workspace:") {
			t.Errorf("--dir read %s with %s, prompt %q", read("dir"), read("files"), read("prompt"))
		}
	})

	t.Run("one-shot snapshot is removed", func(t *testing.T) {
		c3 := commitToOrigin(t, seed, "v3")
		parent := filepath.Join(e.home, ".klaus", "consult-snapshots")
		out, err := e.run(t, "--backend", "codex", "--repo", "widget", "question")
		if err != nil {
			t.Fatalf("consult: %s, %v", out, err)
		}
		if dir := read("dir"); !strings.HasPrefix(dir, parent+"/") || filepath.Base(dir) != "widget" || read("main.txt") != "v3" || !strings.Contains(read("prompt"), c3) {
			t.Errorf("one-shot read %s: %s, prompt %q", dir, read("main.txt"), read("prompt"))
		}
		if left, _ := os.ReadDir(parent); len(left) != 0 {
			t.Errorf("one-shot snapshot left behind: %v", left)
		}
		if list := snapshotGit(t, clone, "worktree", "list", "--porcelain"); strings.Contains(list, parent) {
			t.Errorf("clone still lists the one-shot snapshot:\n%s", list)
		}
	})

	t.Run("panel members share a snapshot", func(t *testing.T) {
		for _, kind := range []string{"claude", "agy"} {
			e.stub(t, kind, "pwd -P > "+rec+"/dir."+kind+"\ncat >/dev/null\necho an answer\n")
		}
		out, err := e.run(t, "--panel", "--repo", "widget", "question")
		if err != nil {
			t.Fatalf("panel: %s, %v", out, err)
		}
		// KLAUS_BACKEND=agy, so the panel is codex and claude.
		if read("dir.claude") != read("dir") || filepath.Base(read("dir")) != "widget" {
			t.Errorf("panel members read %s and %s", read("dir"), read("dir.claude"))
		}
	})

	t.Run("offline fetch reads the last snapshot", func(t *testing.T) {
		if saved, _ = store.Load("design"); saved.Commit != c2 {
			t.Fatalf("thread commit = %s, want %s", saved.Commit, c2)
		}
		if err := os.Rename(origin, origin+".off"); err != nil {
			t.Fatal(err)
		}
		defer os.Rename(origin+".off", origin)
		out := turn(t, c2, "v2", snap, "--thread", "design", "offline question")
		if !strings.Contains(out, "warning: fetching origin/main of project \"widget\" failed") || !strings.Contains(out, "reading the last snapshot, at "+c2) {
			t.Errorf("no fallback warning: %s", out)
		}
		out, err := e.run(t, "--backend", "codex", "--repo", "widget", "question")
		if err == nil || !strings.Contains(out, "there is no earlier snapshot to read") {
			t.Errorf("one-shot without a snapshot: %s, %v", out, err)
		}
	})

	t.Run("cleanup removes the thread snapshot", func(t *testing.T) {
		c := exec.Command(bin, "cleanup", "--all")
		c.Env = e.command().Env
		out, err := c.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Removed consult thread design's snapshot") {
			t.Fatalf("cleanup: %s, %v", out, err)
		}
		if _, err := os.Stat(snap); !os.IsNotExist(err) {
			t.Errorf("snapshot still at %s: %v", snap, err)
		}
		if list := snapshotGit(t, clone, "worktree", "list", "--porcelain"); strings.Contains(list, snap) {
			t.Errorf("clone still lists the snapshot:\n%s", list)
		}
	})
}

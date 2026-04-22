package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/project"
)

func runGitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setupProjectWithUpstream creates a bare origin and a clone, and returns the
// clone path. Advances origin by one commit if advance=true.
func setupProjectWithUpstream(t *testing.T, advance bool) string {
	t.Helper()
	seed := t.TempDir()
	runGitT(t, ".", "init", "--initial-branch=main", seed)
	runGitT(t, seed, "config", "user.email", "t@t.com")
	runGitT(t, seed, "config", "user.name", "T")
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644)
	runGitT(t, seed, "add", "README.md")
	runGitT(t, seed, "commit", "-m", "init")

	bare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "clone", "--bare", seed, bare).CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v\n%s", err, out)
	}

	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", bare, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	runGitT(t, clone, "config", "user.email", "t@t.com")
	runGitT(t, clone, "config", "user.name", "T")

	if advance {
		staging := filepath.Join(t.TempDir(), "staging")
		if out, err := exec.Command("git", "clone", bare, staging).CombinedOutput(); err != nil {
			t.Fatalf("staging clone: %v\n%s", err, out)
		}
		runGitT(t, staging, "config", "user.email", "t@t.com")
		runGitT(t, staging, "config", "user.name", "T")
		os.WriteFile(filepath.Join(staging, "extra.txt"), []byte("x"), 0o644)
		runGitT(t, staging, "add", "extra.txt")
		runGitT(t, staging, "commit", "-m", "advance")
		runGitT(t, staging, "push", "origin", "main")
	}

	return clone
}

// withTempHome sets HOME to a temp dir for the duration of the test and
// registers cleanup.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// writeRegistry writes a projects.json inside HOME/.klaus pointing at the
// given projects.
func writeRegistry(t *testing.T, home string, projects map[string]string) {
	t.Helper()
	reg := &project.Registry{
		ProjectsDir: filepath.Join(home, "src"),
		Projects:    projects,
	}
	regDir := filepath.Join(home, ".klaus")
	if err := os.MkdirAll(regDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reg.SaveTo(filepath.Join(regDir, "projects.json")); err != nil {
		t.Fatal(err)
	}
}

func TestSyncCmd_TableOutputAndExit(t *testing.T) {
	home := withTempHome(t)
	cleanClone := setupProjectWithUpstream(t, true) // behind upstream, clean

	writeRegistry(t, home, map[string]string{
		"demo": cleanClone,
	})

	buf := &bytes.Buffer{}
	cmd := syncCmd
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{}); err != nil {
		t.Fatalf("syncCmd: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "demo") {
		t.Errorf("output should include project name, got:\n%s", out)
	}
	if !strings.Contains(out, "pulled") {
		t.Errorf("output should include 'pulled' status, got:\n%s", out)
	}
	if !strings.Contains(out, "main") {
		t.Errorf("output should include branch name, got:\n%s", out)
	}
}

func TestSyncCmd_DirtyIsSkippedNoError(t *testing.T) {
	home := withTempHome(t)
	clone := setupProjectWithUpstream(t, true)

	// Dirty the clone
	os.WriteFile(filepath.Join(clone, "wip.txt"), []byte("wip"), 0o644)

	writeRegistry(t, home, map[string]string{"demo": clone})

	buf := &bytes.Buffer{}
	cmd := syncCmd
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{})
	if err != nil {
		t.Errorf("dirty clone should NOT cause a non-zero exit, got err: %v", err)
	}
	if !strings.Contains(buf.String(), "skipped") {
		t.Errorf("output should include 'skipped', got:\n%s", buf.String())
	}
}

func TestSyncCmd_FetchErrorReturnsError(t *testing.T) {
	home := withTempHome(t)
	clone := setupProjectWithUpstream(t, false)

	// Break the remote so fetch fails
	runGitT(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "nope.git"))

	writeRegistry(t, home, map[string]string{"demo": clone})

	buf := &bytes.Buffer{}
	cmd := syncCmd
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{})
	if err == nil {
		t.Error("expected error exit when a project fetch fails")
	}
	if !strings.Contains(buf.String(), "error") {
		t.Errorf("output should include 'error', got:\n%s", buf.String())
	}
}

func TestSyncCmd_EmptyRegistry(t *testing.T) {
	home := withTempHome(t)
	writeRegistry(t, home, map[string]string{})

	buf := &bytes.Buffer{}
	cmd := syncCmd
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{}); err != nil {
		t.Fatalf("empty registry: %v", err)
	}
	if !strings.Contains(buf.String(), "No projects") {
		t.Errorf("expected 'No projects' message, got:\n%s", buf.String())
	}
}

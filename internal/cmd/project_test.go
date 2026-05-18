package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/project"
)

// withStubProjectDeps swaps getProjectDeps for a stub that returns the given
// description (and no-op clone/resolve). Restores on test cleanup.
func withStubProjectDeps(t *testing.T, description string) {
	t.Helper()
	prev := getProjectDeps
	getProjectDeps = func() ProjectDeps {
		return ProjectDeps{
			ResolveGitHubRepo: func(name string) (string, string, error) {
				return "stub-owner", "https://example.invalid/stub.git", nil
			},
			GitClone: func(url, targetDir string) error {
				return initGitRepoErr(targetDir)
			},
			FetchDescription: func(repoSlug string) string {
				return description
			},
		}
	}
	t.Cleanup(func() { getProjectDeps = prev })
}

// initGitRepoErr is the error-returning sibling of initGitRepo for use inside
// stub GitClone implementations (where we don't have a *testing.T).
func initGitRepoErr(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		return err
	}
	return nil
}

// invokeProjectAdd resets the projectAddCmd flags and calls runProjectAdd
// directly. ref is the positional argument; path/description are flag values
// ("" leaves the flag unset). Returns the captured stdout/stderr.
func invokeProjectAdd(t *testing.T, ref, path, description string) string {
	t.Helper()
	// Reset flags so values don't leak between tests.
	if err := projectAddCmd.Flags().Set("path", path); err != nil {
		t.Fatal(err)
	}
	if err := projectAddCmd.Flags().Set("description", description); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = projectAddCmd.Flags().Set("path", "")
		_ = projectAddCmd.Flags().Set("description", "")
	})

	buf := &bytes.Buffer{}
	projectAddCmd.SetOut(buf)
	projectAddCmd.SetErr(buf)
	if err := runProjectAdd(projectAddCmd, []string{ref}); err != nil {
		t.Fatalf("runProjectAdd(%q): %v\n%s", ref, err, buf.String())
	}
	return buf.String()
}

func TestProjectAddExplicitDescription(t *testing.T) {
	home := withTempHome(t)
	withStubProjectDeps(t, "should-not-be-used")

	repoDir := filepath.Join(t.TempDir(), "mytool")
	initGitRepo(t, repoDir)

	out := invokeProjectAdd(t, "owner/mytool", repoDir, "my cool tool")
	if !strings.Contains(out, "my cool tool") {
		t.Errorf("expected output to mention description, got: %q", out)
	}

	reg, err := project.LoadFrom(filepath.Join(home, ".klaus", "projects.json"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := reg.Description("mytool"); got != "my cool tool" {
		t.Errorf("Description = %q, want %q", got, "my cool tool")
	}
	if _, ok := reg.Get("mytool"); !ok {
		t.Error("project mytool not registered")
	}
}

func TestProjectAddGhDescriptionFallback(t *testing.T) {
	home := withTempHome(t)
	withStubProjectDeps(t, "from-github")

	repoDir := filepath.Join(t.TempDir(), "mytool")
	initGitRepo(t, repoDir)

	out := invokeProjectAdd(t, "owner/mytool", repoDir, "")
	if !strings.Contains(out, "from-github") {
		t.Errorf("expected output to include fallback description, got: %q", out)
	}

	reg, err := project.LoadFrom(filepath.Join(home, ".klaus", "projects.json"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := reg.Description("mytool"); got != "from-github" {
		t.Errorf("Description = %q, want %q", got, "from-github")
	}
}

func TestProjectAddNoDescriptionWhenGhEmpty(t *testing.T) {
	home := withTempHome(t)
	withStubProjectDeps(t, "") // gh returns empty -> no description

	repoDir := filepath.Join(t.TempDir(), "mytool")
	initGitRepo(t, repoDir)

	out := invokeProjectAdd(t, "owner/mytool", repoDir, "")
	// The output should not include an em dash since no description.
	if strings.Contains(out, "—") {
		t.Errorf("unexpected em-dash in output: %q", out)
	}

	reg, err := project.LoadFrom(filepath.Join(home, ".klaus", "projects.json"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := reg.Description("mytool"); got != "" {
		t.Errorf("Description = %q, want empty", got)
	}
	if _, ok := reg.Get("mytool"); !ok {
		t.Error("project mytool not registered")
	}
}

func TestProjectAddExplicitOverridesGh(t *testing.T) {
	// --description should win over the gh fetch.
	home := withTempHome(t)
	withStubProjectDeps(t, "from-github")

	repoDir := filepath.Join(t.TempDir(), "mytool")
	initGitRepo(t, repoDir)

	invokeProjectAdd(t, "owner/mytool", repoDir, "explicit wins")

	reg, err := project.LoadFrom(filepath.Join(home, ".klaus", "projects.json"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := reg.Description("mytool"); got != "explicit wins" {
		t.Errorf("Description = %q, want %q", got, "explicit wins")
	}
}

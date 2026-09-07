package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initRepo creates a real git repo at dir with the given origin URL.
func initRepo(t *testing.T, dir, origin string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"remote", "add", "origin", origin},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// macOS resolves TMPDIR through a symlink; git reports the real path.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving %s: %v", dir, err)
	}
	return real
}

func TestExistingRepoDirUsesCurrentCheckout(t *testing.T) {
	dir := initRepo(t, filepath.Join(t.TempDir(), "widget"), "https://github.com/acme/widget.git")
	writeRegistry(t, withTempHome(t), nil)
	t.Chdir(dir)

	// No reference at all: whatever repo we are standing in.
	if got := existingRepoDir(""); got != dir {
		t.Errorf("existingRepoDir(\"\") = %q, want %q", got, dir)
	}
	// A reference the current checkout demonstrably satisfies.
	if got := existingRepoDir("acme/widget"); got != dir {
		t.Errorf("existingRepoDir(\"acme/widget\") = %q, want %q", got, dir)
	}
	// A different repo must not silently resolve to the current checkout.
	if got := existingRepoDir("acme/gadget"); got != "" {
		t.Errorf("existingRepoDir(\"acme/gadget\") = %q, want \"\"", got)
	}
	// Nor may a bare name, which says nothing about this checkout's origin.
	if got := existingRepoDir("widget"); got != "" {
		t.Errorf("existingRepoDir(\"widget\") = %q, want \"\"", got)
	}
}

func TestExistingRepoDirFindsRegisteredProject(t *testing.T) {
	root := t.TempDir()
	widget := initRepo(t, filepath.Join(root, "widget"), "git@github.com:acme/widget.git")
	local := initRepo(t, filepath.Join(root, "gizmo"), filepath.Join(root, "gizmo-origin.git"))
	writeRegistry(t, withTempHome(t), map[string]string{"widget": widget, "gizmo": local})
	// Outside any checkout, so only the registry can answer.
	t.Chdir(root)

	// Matched by origin slug, in every reference form.
	for _, ref := range []string{"acme/widget", "https://github.com/acme/widget", "git@github.com:acme/widget.git"} {
		if got := existingRepoDir(ref); got != widget {
			t.Errorf("existingRepoDir(%q) = %q, want %q", ref, got, widget)
		}
	}
	// A project whose origin is not a GitHub URL can only be matched by name.
	if got := existingRepoDir("gizmo"); got != local {
		t.Errorf("existingRepoDir(\"gizmo\") = %q, want %q", got, local)
	}
	// A GitHub repo nobody has checked out stays unresolved (no cloning here).
	if got := existingRepoDir("acme/unknown"); got != "" {
		t.Errorf("existingRepoDir(\"acme/unknown\") = %q, want \"\"", got)
	}
}

// A project registered under a name that collides with a different GitHub repo
// must not be handed back for that repo — the origin disagrees.
func TestExistingRepoDirIgnoresNameCollisionWithOtherOrigin(t *testing.T) {
	root := t.TempDir()
	widget := initRepo(t, filepath.Join(root, "widget"), "https://github.com/acme/widget.git")
	writeRegistry(t, withTempHome(t), map[string]string{"widget": widget})
	t.Chdir(root)

	if got := existingRepoDir("other/widget"); got != "" {
		t.Errorf("existingRepoDir(\"other/widget\") = %q, want \"\"", got)
	}
}

func TestResolveRepoDirWithoutTargetOutsideRepo(t *testing.T) {
	writeRegistry(t, withTempHome(t), nil)
	t.Chdir(t.TempDir())

	_, err := resolveRepoDirUncached(context.Background(), "")
	if !errors.Is(err, errNoTargetRepo) {
		t.Errorf("error = %v, want errNoTargetRepo", err)
	}
}

func TestRepoSlug(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{"acme/widget", "acme/widget"},
		{"https://github.com/acme/widget.git", "acme/widget"},
		{"git@github.com:acme/widget", "acme/widget"},
		{"widget", ""},              // bare project name, no owner
		{"", ""},                    // nothing to resolve
		{"/srv/git/widget.git", ""}, // a local path, not a GitHub repo
	}
	for _, tt := range tests {
		if got := repoSlug(tt.ref); got != tt.want {
			t.Errorf("repoSlug(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

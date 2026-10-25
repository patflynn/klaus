package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/config"
)

// forbiddenScanRepo is a real git repo whose main already holds a term (in
// hosts.md) and a dotted address (in net.md), checked out on branch work.
func forbiddenScanRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q", "-b", "main")
	runGitT(t, dir, "config", "user.email", "t@example.com")
	runGitT(t, dir, "config", "user.name", "T")
	writeScanFile(t, dir, "hosts.md", "first\nexample-host is old\nlast\n")
	writeScanFile(t, dir, "net.md", "one\n")
	runGitT(t, dir, "add", "-A")
	runGitT(t, dir, "commit", "-qm", "init")
	runGitT(t, dir, "checkout", "-qb", "work")
	return dir
}

func writeScanFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func headShort(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%h").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestScanForbiddenTerms(t *testing.T) {
	entry := config.ForbiddenTermsConfig{
		Terms:      []string{"example-host", "example-project", "10.1.2."},
		AllowPaths: []string{"docs/allowed", "exempt.md", "docs/host notes.md"},
	}
	tests := []struct {
		name   string
		change func(t *testing.T, dir string) // commits on work
		want   func(dir string) []string
	}{
		{
			name: "added line",
			change: func(t *testing.T, dir string) {
				writeScanFile(t, dir, "net.md", "one\nreach example-host here\n")
				runGitT(t, dir, "commit", "-qam", "add a line")
			},
			want: func(string) []string { return []string{"net.md:2: example-host"} },
		},
		{
			name: "deleted line",
			change: func(t *testing.T, dir string) {
				writeScanFile(t, dir, "hosts.md", "first\nlast\n")
				runGitT(t, dir, "commit", "-qam", "drop the old line")
			},
		},
		{
			name: "allow-listed paths",
			change: func(t *testing.T, dir string) {
				writeScanFile(t, dir, "exempt.md", "example-host\n")
				writeScanFile(t, dir, "docs/allowed/a.md", "example-host\n")
				writeScanFile(t, dir, "docs/allowed-not/a.md", "x\nexample-host\n")
				runGitT(t, dir, "add", "-A")
				runGitT(t, dir, "commit", "-qm", "exempt files")
			},
			// A directory prefix covers what is under it, not its namesakes.
			want: func(string) []string { return []string{"docs/allowed-not/a.md:2: example-host"} },
		},
		{
			name: "file moved out of allow_paths",
			change: func(t *testing.T, dir string) {
				runGitT(t, dir, "checkout", "-q", "main")
				writeScanFile(t, dir, "exempt.md", "example-host\n")
				runGitT(t, dir, "add", "-A")
				runGitT(t, dir, "commit", "-qm", "exempt file")
				runGitT(t, dir, "checkout", "-q", "work")
				runGitT(t, dir, "merge", "-q", "--ff-only", "main")
				runGitT(t, dir, "mv", "exempt.md", "moved.md")
				runGitT(t, dir, "commit", "-qm", "move it")
			},
			want: func(string) []string { return []string{"moved.md:1: example-host"} },
		},
		{
			name: "paths with spaces",
			change: func(t *testing.T, dir string) {
				writeScanFile(t, dir, "docs/host notes.md", "example-host\n")
				writeScanFile(t, dir, "my notes.md", "example-host\n")
				runGitT(t, dir, "add", "-A")
				runGitT(t, dir, "commit", "-qm", "notes")
			},
			want: func(string) []string { return []string{"my notes.md:1: example-host"} },
		},
		{
			name: "file git won't diff",
			change: func(t *testing.T, dir string) {
				writeScanFile(t, dir, ".gitattributes", "*.dat -diff\n")
				writeScanFile(t, dir, "hosts.dat", "a\x00b\nexample-host\n")
				runGitT(t, dir, "add", "-A")
				runGitT(t, dir, "commit", "-qm", "data")
			},
			want: func(string) []string { return []string{"hosts.dat:2: example-host"} },
		},
		{
			name: "commit message",
			change: func(t *testing.T, dir string) {
				writeScanFile(t, dir, "net.md", "one\ntwo\n")
				runGitT(t, dir, "commit", "-qam", "tidy\n\nFor the example-project rollout.")
			},
			want: func(dir string) []string { return []string{"commit " + headShort(t, dir) + ": example-project"} },
		},
		{
			name: "case-insensitive",
			change: func(t *testing.T, dir string) {
				writeScanFile(t, dir, "net.md", "one\nThe EXAMPLE-Host box\n")
				runGitT(t, dir, "commit", "-qam", "shout")
			},
			want: func(string) []string { return []string{"net.md:2: example-host"} },
		},
		{
			name: "dots are literal",
			change: func(t *testing.T, dir string) {
				writeScanFile(t, dir, "net.md", "one\nroute 10.1.20.0/24\nvia 10x1x2x3\ngateway 10.1.2.7\n")
				runGitT(t, dir, "commit", "-qam", "routes")
			},
			want: func(string) []string { return []string{"net.md:4: 10.1.2."} },
		},
		{
			name: "added file name",
			change: func(t *testing.T, dir string) {
				writeScanFile(t, dir, "notes/example-host.md", "clean\n")
				runGitT(t, dir, "add", "-A")
				runGitT(t, dir, "commit", "-qm", "notes")
			},
			want: func(string) []string { return []string{"notes/example-host.md: example-host (file name)"} },
		},
		{
			name: "added line that looks like a file header",
			change: func(t *testing.T, dir string) {
				writeScanFile(t, dir, "net.md", "one\n++ b/example-host\n")
				runGitT(t, dir, "commit", "-qam", "odd line")
			},
			want: func(string) []string { return []string{"net.md:2: example-host"} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := forbiddenScanRepo(t)
			tt.change(t, dir)
			hits, err := scanForbiddenTerms(dir, "main", "HEAD", []config.ForbiddenTermsConfig{entry})
			if err != nil {
				t.Fatalf("scanForbiddenTerms: %v", err)
			}
			var got []string
			for _, h := range hits {
				got = append(got, h.String())
			}
			var want []string
			if tt.want != nil {
				want = tt.want(dir)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("hits = %q, want %q", got, want)
			}
		})
	}
}

// An entry applies to a repo by its owner/repo origin, or by its registered
// project name from any worktree of that project's checkout.
func TestForbiddenTermsForMatchesRepo(t *testing.T) {
	root := t.TempDir()
	widget := initRepo(t, filepath.Join(root, "widget"), "git@github.com:acme/widget.git")
	runGitT(t, widget, "-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(root, "wt")
	runGitT(t, widget, "worktree", "add", "-q", "-b", "work", wt)
	local := initRepo(t, filepath.Join(root, "gizmo"), filepath.Join(root, "gizmo-origin.git"))
	writeRegistry(t, withTempHome(t), map[string]string{"example-project": local, "widget": widget})

	terms := func(ts ...string) config.ForbiddenTermsConfig { return config.ForbiddenTermsConfig{Terms: ts} }
	tests := []struct {
		name    string
		entries map[string]config.ForbiddenTermsConfig
		dir     string
		repoRef string
		want    []config.ForbiddenTermsConfig
	}{
		{"owner/repo origin", map[string]config.ForbiddenTermsConfig{"Acme/Widget": terms("a")}, wt, "", []config.ForbiddenTermsConfig{terms("a")}},
		{"project of the worktree", map[string]config.ForbiddenTermsConfig{"widget": terms("a")}, wt, "", []config.ForbiddenTermsConfig{terms("a")}},
		{"both keys apply", map[string]config.ForbiddenTermsConfig{"acme/widget": terms("a"), "widget": terms("b")}, wt, "", []config.ForbiddenTermsConfig{terms("a"), terms("b")}},
		{"project without a GitHub origin", map[string]config.ForbiddenTermsConfig{"example-project": terms("a")}, local, "", []config.ForbiddenTermsConfig{terms("a")}},
		{"other repos' entries", map[string]config.ForbiddenTermsConfig{"acme/gadget": terms("a"), "example-project": terms("b")}, wt, "", nil},
		{"--repo by project name", map[string]config.ForbiddenTermsConfig{"acme/widget": terms("a")}, local, "widget", []config.ForbiddenTermsConfig{terms("a")}},
		{"--repo by owner/repo", map[string]config.ForbiddenTermsConfig{"widget": terms("a")}, local, "acme/widget", []config.ForbiddenTermsConfig{terms("a")}},
		{"--repo unregistered", map[string]config.ForbiddenTermsConfig{"elsewhere": terms("a")}, local, "elsewhere", []config.ForbiddenTermsConfig{terms("a")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forbiddenTermsFor(config.Config{ForbiddenTerms: tt.entries}, tt.dir, tt.repoRef)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("forbiddenTermsFor = %+v, want %+v", got, tt.want)
			}
		})
	}
}

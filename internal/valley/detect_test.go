package valley

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newHelp is how a valley with change records lists its verbs.
const newHelp = `valley — integrator verbs for a valley project

usage: valley <verb> [args]

  pending          list branches on origin not merged into origin/main
  ready --change <id> --head <sha> <branch>
                   mark a head ready as a version of a change
  changes [--all] [--json]
                   list change records
  approve <change> file an integration request
`

// oldHelp is a valley from before change records: no ready verb, and
// "changes" only in prose.
const oldHelp = `valley — S1 integrator verbs for a valley project

usage: valley <verb> [args]

  pending          list branches on origin not merged into origin/main
  review <branch>  page through the branch's log and diff vs origin/main;
                   approving runs every required check and
                   changes nothing required
  approve <change> file an integration request
`

// TestDetect runs detection against real git: a repo with a non-GitHub
// origin whose commits do and don't carry the valley's policy, with a fake
// valley on PATH.
func TestDetect(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	root := t.TempDir()
	origin, repo := filepath.Join(root, "origin.git"), filepath.Join(root, "repo")
	runGit(t, root, "init", "-q", "--bare", "-b", "main", origin)
	runGit(t, root, "clone", "-q", origin, repo)
	commit := func(path, content string) string {
		t.Helper()
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "add", path)
		runGit(t, repo, "commit", "-q", "-m", "add "+path)
		return runGit(t, repo, "rev-parse", "HEAD")
	}
	plain := commit("README", "hello\n")
	withValley := commit("policy/valley", "cosmo\n")
	runGit(t, repo, "rm", "-q", "policy/valley")
	runGit(t, repo, "commit", "-q", "-m", "drop the policy")
	withInstance := commit("policy/instance/host.cue", "host: \"x\"\n")
	runGit(t, repo, "rm", "-q", "-r", "policy")
	runGit(t, repo, "commit", "-q", "-m", "drop it again")
	instanceFile := commit("policy/instance", "not a directory\n")

	// A head pushed from elsewhere (a sandbox) that this clone hasn't
	// fetched: detection fetches its branch.
	other := filepath.Join(root, "other")
	runGit(t, root, "clone", "-q", repo, other)
	runGit(t, other, "remote", "set-url", "origin", origin)
	runGit(t, repo, "push", "-q", "origin", "HEAD:main")
	runGit(t, other, "fetch", "-q", "origin")
	runGit(t, other, "checkout", "-q", "-b", "agent/sandbox", "origin/main")
	if err := os.MkdirAll(filepath.Join(other, "policy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "policy", "valley"), []byte("cosmo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, other, "add", "policy")
	runGit(t, other, "commit", "-q", "-m", "from the sandbox")
	runGit(t, other, "push", "-q", "origin", "agent/sandbox")
	sandboxHead := runGit(t, other, "rev-parse", "HEAD")

	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeValley := func(help string) {
		t.Helper()
		script := "#!/bin/sh\ncat <<'EOF'\n" + help + "EOF\n"
		if err := os.WriteFile(filepath.Join(bin, "valley"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	fakeValley(newHelp)
	for _, tc := range []struct {
		name   string
		branch string
		head   string
		want   bool
	}{
		{"no policy", "main", plain, false},
		{"policy/valley", "main", withValley, true},
		{"policy/instance/", "main", withInstance, true},
		{"policy/instance is a file", "main", instanceFile, false},
		{"head origin lacks too", "main", "0123456789abcdef0123456789abcdef01234567", false},
		{"head pushed from a sandbox", "agent/sandbox", sandboxHead, true},
	} {
		if got := Detect(ctx, repo, tc.branch, tc.head); got != tc.want {
			t.Errorf("%s: Detect = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A valley from before change records lacks the verbs: not a valley.
	fakeValley(oldHelp)
	if Detect(ctx, repo, "main", withValley) {
		t.Error("a valley without ready and changes should not be detected")
	}

	// Returning a change to draft needs unready too, which a valley from
	// before drafts lacks.
	fakeValley(newHelp)
	if DetectUnready(ctx, repo, withValley) {
		t.Error("a valley without unready should not be detected for unready")
	}
	fakeValley(newHelp + "  unready <change>  return a ready change to draft\n")
	if !DetectUnready(ctx, repo, withValley) {
		t.Error("a valley that lists unready should be detected for unready")
	}
	if DetectUnready(ctx, repo, plain) {
		t.Error("a head without the policy should not be detected for unready")
	}

	// A GitHub origin gets PRs, whatever the tree carries.
	fakeValley(newHelp)
	runGit(t, repo, "remote", "set-url", "origin", "git@github.com:acme/cosmo.git")
	if Detect(ctx, repo, "main", withValley) {
		t.Error("a GitHub origin should not be detected as a valley")
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

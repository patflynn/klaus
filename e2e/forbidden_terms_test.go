//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbidTerms registers the sandbox repo as project example-project and
// writes a host config whose forbidden_terms has an entry under key.
func (h *Harness) forbidTerms(key string) {
	h.t.Helper()
	h.RegisterProject("acme/example-project", h.RepoDir)
	cfg := map[string]any{"forbidden_terms": map[string]any{
		key: map[string]any{"terms": []string{"example-host", "10.1.2."}, "allow_paths": []string{"docs/hosts"}},
	}}
	data, _ := json.Marshal(cfg)
	if err := os.MkdirAll(filepath.Join(h.Home, ".klaus"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Home, ".klaus", "config.json"), data, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// stubReviewer makes _pre-review's peer review a claude stub that finds
// nothing, and returns the file the stub creates when it runs.
func (h *Harness) stubReviewer() string {
	h.t.Helper()
	marker := filepath.Join(h.E2EDir, "reviewed")
	h.WriteStub("claude", "#!/usr/bin/env bash\ncat >/dev/null\ntouch "+marker+"\necho '{\"findings\": [], \"summary\": \"No issues found.\"}'\n")
	h.AmendRepoConfig(map[string]any{"backends": map[string]any{"claude": map[string]any{"review_backend": "claude"}}})
	return marker
}

// commitReadme rewrites README.md and commits it with message.
func (h *Harness) commitReadme(content, message string) string {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.RepoDir, "README.md"), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.git(h.RepoDir, "commit", "-qam", message)
	return h.GitOutput(h.RepoDir, "rev-parse", "HEAD")
}

// TestPreReviewRefusesForbiddenTerms: with an entry for the repo in the host
// config, _pre-review fails a branch that adds a forbidden term, naming the
// file, line and term but not the rest of the list, before the peer
// reviewer sees the diff. Once the term is replaced it runs the review.
func TestPreReviewRefusesForbiddenTerms(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.forbidTerms("example-project")
	reviewed := h.stubReviewer()
	h.git(h.RepoDir, "checkout", "-qb", "work")
	h.commitReadme("# widget\nDeploy to Example-Host first.\n", "add deploy note")

	res := h.RunKlaus("_pre-review")
	if res.ExitCode != 1 {
		t.Fatalf("_pre-review exit = %d, want 1\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	for _, want := range []string{"README.md:2: example-host", "replace each with a neutral name", "PR creation should be blocked"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("_pre-review output lacks %q:\n%s", want, res.Stdout)
		}
	}
	if strings.Contains(res.Stdout, "10.1.2.") {
		t.Errorf("_pre-review printed a term that didn't hit:\n%s", res.Stdout)
	}
	if _, err := os.Stat(reviewed); err == nil {
		t.Error("the peer reviewer ran on a diff with a forbidden term")
	}

	h.commitReadme("# widget\nDeploy to the build host first.\n", "use a neutral name")
	res = h.RunKlaus("_pre-review")
	if res.ExitCode != 0 {
		t.Fatalf("_pre-review after the fix: exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	for _, want := range []string{"✓ no forbidden terms added", "All checks passed."} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("_pre-review output lacks %q:\n%s", want, res.Stdout)
		}
	}
	if _, err := os.Stat(reviewed); err != nil {
		t.Error("the peer reviewer didn't run once the term was gone")
	}
}

// TestPreReviewIgnoresOtherReposTerms: an entry for another repo leaves
// _pre-review as it was: no forbidden-terms check, the review runs.
func TestPreReviewIgnoresOtherReposTerms(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.forbidTerms("acme/gadget")
	reviewed := h.stubReviewer()
	h.git(h.RepoDir, "checkout", "-qb", "work")
	h.commitReadme("# widget\nDeploy to example-host first.\n", "add deploy note")

	res := h.RunKlaus("_pre-review")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "All checks passed.") {
		t.Fatalf("_pre-review exit = %d, want 0 and all checks passed\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stdout, "orbidden") {
		t.Errorf("_pre-review checked forbidden terms for a repo without an entry:\n%s", res.Stdout)
	}
	if _, err := os.Stat(reviewed); err != nil {
		t.Error("the peer reviewer didn't run")
	}
}

// TestForbiddenTermsCommand: klaus _forbidden-terms scans the range it is
// given, defaults to the change since origin's default branch, catches a
// term in a commit message, and passes a repo --repo says has no entry.
func TestForbiddenTermsCommand(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.forbidTerms("example-project")
	c0 := h.GitOutput(h.RepoDir, "rev-parse", "HEAD")
	c1 := h.commitReadme("# widget\nDeploy to example-host first.\n", "add deploy note")
	c2 := h.commitReadme("# widget\nDeploy to example-host first.\nThen check it.\n", "add a check")
	h.git(h.RepoDir, "commit", "-q", "--allow-empty", "-m", "Move EXAMPLE-HOST behind the proxy")
	c3 := h.GitOutput(h.RepoDir, "rev-parse", "HEAD")

	for _, tc := range []struct {
		name string
		args []string
		exit int
		want string
	}{
		{"range that adds a term", []string{c0, c1}, 1, "README.md:2: example-host"},
		{"range after it", []string{c1, c2}, 0, "✓ no forbidden terms added"},
		{"commit message", []string{c2, c3}, 1, "commit " + c3[:7]},
		{"default range", nil, 1, "README.md:2: example-host"},
		{"repo without an entry", []string{"--repo", "acme/gadget", c0, c1}, 0, "No forbidden terms are configured for this repo."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := h.RunKlaus(append([]string{"_forbidden-terms"}, tc.args...)...)
			if res.ExitCode != tc.exit || !strings.Contains(res.Stdout, tc.want) {
				t.Errorf("_forbidden-terms %v: exit %d, want %d with %q\n%s%s", tc.args, res.ExitCode, tc.exit, tc.want, res.Stdout, res.Stderr)
			}
		})
	}
}

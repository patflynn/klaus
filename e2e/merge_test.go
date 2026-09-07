//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mergeGHStub returns a gh stub that answers every query `klaus merge` makes
// for one approved, CI-green, behind-but-clean PR. mergeOutcome scripts what
// `gh pr merge` does: "ok" always succeeds, "stale-then-ok" refuses the first
// attempt the way GitHub refuses a branch that must be up to date, then
// succeeds.
func mergeGHStub(h *Harness, mergeOutcome string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
dir=%q
{ printf 'gh'; for a in "$@"; do printf ' %%q' "$a"; done; printf '\n'; } >> "$dir/gh.argv"
case "$*" in
  *"pr merge"*)
    n=$(cat "$dir/merge.count" 2>/dev/null || echo 0)
    echo $((n + 1)) > "$dir/merge.count"
    if [ "%s" = stale-then-ok ] && [ "$n" = 0 ]; then
      echo "failed to merge: Base branch policy prohibits the merge" >&2
      exit 1
    fi
    exit 0 ;;
  *update-branch*) exit 0 ;;
  *"pr checks"*) echo "build	pass	1s	https://ci/1" ; exit 0 ;;
  *"--json title"*) echo "Fix the widget" ; exit 0 ;;
  *"--json mergeable"*) echo MERGEABLE ; exit 0 ;;
  *"--json baseRefName"*) printf 'main\tfeature\tacme\n' ; exit 0 ;;
  *"--json reviewDecision"*) echo APPROVED ; exit 0 ;;
  *"--json headRefName"*) echo feature ; exit 0 ;;
  *compare*) echo 2 ; exit 0 ;;
  *"repo view"*) echo acme/widget ; exit 0 ;;
esac
exit 0
`, h.E2EDir, mergeOutcome)
}

// mergeCount returns how many times the stubbed `gh pr merge` ran.
func mergeCount(h *Harness) string {
	b, err := os.ReadFile(filepath.Join(h.E2EDir, "merge.count"))
	if err != nil {
		return "0"
	}
	return strings.TrimSpace(string(b))
}

// A branch that is only behind main must be merged as-is. Rebasing it would
// force-push a new head and, under a policy that dismisses reviews on new
// commits, throw away the approval the merge depends on (issue #293).
func TestMergeBehindBranchMergesWithoutRebasing(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteStub("gh", mergeGHStub(h, "ok"))

	res := h.RunKlausIn(h.E2EDir, "merge", "42", "--repo", "acme/widget", "--force")

	if res.ExitCode != 0 {
		t.Fatalf("merge exited %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Merged PR #42") {
		t.Errorf("expected the PR to merge, got: %s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "Behind: 2") {
		t.Errorf("expected the behind count to be reported, got: %s", res.Stdout)
	}
	if strings.Contains(strings.ToLower(res.Stdout), "rebas") {
		t.Errorf("a behind-but-clean branch must not be rebased, got: %s", res.Stdout)
	}
	if got := h.GHArgv(); strings.Contains(got, "update-branch") {
		t.Errorf("update-branch is only for a refused merge, got: %s", got)
	}
	if got := mergeCount(h); got != "1" {
		t.Errorf("gh pr merge ran %s times, want 1", got)
	}
}

// When GitHub refuses the merge because the branch must be up to date, klaus
// falls back to the server-side "Update branch" (a merge from main, which keeps
// the head — and the approval — intact), waits for CI, and merges again.
func TestMergeFallsBackToUpdateBranchOnStaleRefusal(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteStub("gh", mergeGHStub(h, "stale-then-ok"))

	res := h.RunKlausIn(h.E2EDir, "merge", "42", "--repo", "acme/widget", "--force")

	if res.ExitCode != 0 {
		t.Fatalf("merge exited %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "updating branch from main") {
		t.Errorf("expected the update-branch fallback, got: %s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "Merged PR #42") {
		t.Errorf("expected the PR to merge after the update, got: %s", res.Stdout)
	}
	if strings.Contains(strings.ToLower(res.Stdout), "rebas") {
		t.Errorf("the fallback must not rebase, got: %s", res.Stdout)
	}
	argv := h.GHArgv()
	if !strings.Contains(argv, "pr update-branch") {
		t.Errorf("expected the server-side update-branch call, got: %s", argv)
	}
	if got := mergeCount(h); got != "2" {
		t.Errorf("gh pr merge ran %s times, want 2 (refused, then retried)", got)
	}
}

// merge must work outside a git checkout, resolving the target repo's config
// (here require_approval=false) rather than the current directory's. Without
// that resolution the approval default of true would skip the PR (issue #293).
func TestMergeOutsideGitRepoUsesTargetRepoConfig(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteStub("gh", mergeGHStub(h, "ok"))
	h.AmendRepoConfig(map[string]any{"require_approval": false})
	// Registers under the repo portion of the ref, i.e. the project name "widget".
	h.RegisterProject("acme/widget", h.RepoDir)

	res := h.RunKlausIn(h.E2EDir, "merge", "42", "--repo", "widget", "--yes")

	if res.ExitCode != 0 {
		t.Fatalf("merge exited %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stdout, "not approved") {
		t.Errorf("target repo config sets require_approval=false, got: %s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "Merged PR #42") {
		t.Errorf("expected the PR to merge, got: %s", res.Stdout)
	}
	if strings.Contains(res.Stderr, "git repository root") {
		t.Errorf("merge must not require a git checkout, got: %s", res.Stderr)
	}
}

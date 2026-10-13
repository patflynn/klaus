//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// reviewerGHStub logs every gh call like the default stub and answers the ones
// launch makes: the gh user, the repo's push collaborators (two pages, as
// --paginate prints them), and the PR lookups of a --pr launch. With
// failCollaborators set, the collaborators call fails as it does for a token
// that cannot read them.
func reviewerGHStub(h *Harness, failCollaborators bool) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
dir=%q
{ printf 'gh'; for a in "$@"; do printf ' %%q' "$a"; done; printf '\n'; } >> "$dir/gh.argv"
case "$*" in
  "api user -q .login") echo acme-agent ;;
  "api --paginate repos/acme/widget/collaborators?permission=push&affiliation=all")
    if %t; then echo "gh: Must have push access to view repository collaborators. (HTTP 403)" >&2; exit 1; fi
    echo '[{"login":"acme-agent","type":"User"},{"login":"alice","type":"User"}]'
    echo '[{"login":"renovate","type":"Bot"},{"login":"acme","type":"User"}]' ;;
  "pr view --json headRefName"*) echo fix-branch ;;
  "pr view --json url"*) echo https://github.com/acme/widget/pull/12 ;;
esac
exit 0
`, h.E2EDir, failCollaborators)
}

// TestLaunchResolvesPRReviewer: with no pr_reviewer configured, a launch that
// will open a GitHub PR asks gh for the repo's push collaborators and requests
// review from the owner; when that gh call fails it warns and launches without
// a reviewer. Runs that don't open a new PR (pr-fix, direct-push) never ask,
// and a configured pr_reviewer is used without running gh at all.
func TestLaunchResolvesPRReviewer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		configured   string // pr_reviewer
		github       bool
		collabFails  bool
		args         []string
		wantReviewer string // "" means the prompt must not request a review
		wantQuery    bool   // collaborators API called
		wantWarning  bool
		wantNoGH     bool
	}{
		{name: "configured pr_reviewer", configured: "carol", github: true, args: []string{"tidy the README"}, wantReviewer: "carol", wantNoGH: true},
		{name: "github origin", github: true, args: []string{"tidy the README"}, wantReviewer: "acme", wantQuery: true},
		{name: "collaborators call fails", github: true, collabFails: true, args: []string{"tidy the README"}, wantQuery: true, wantWarning: true},
		{name: "direct-push origin", args: []string{"tidy the README"}},
		{name: "pr-fix", github: true, args: []string{"--pr", "12", "address the review"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.AmendRepoConfig(map[string]any{"pr_reviewer": tc.configured})
			h.WriteStub("gh", reviewerGHStub(h, tc.collabFails))
			h.git(h.RepoDir, "push", "-q", "origin", "main:refs/heads/fix-branch")
			if tc.github {
				h.UseGitHubOrigin()
			}

			res := h.RunKlaus(append([]string{"launch"}, tc.args...)...)
			if res.ExitCode != 0 {
				t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
			}
			if got := strings.Contains(h.GHArgv(), "/collaborators"); got != tc.wantQuery {
				t.Errorf("collaborators API called = %v, want %v; gh calls:\n%s", got, tc.wantQuery, h.GHArgv())
			}
			if tc.wantNoGH && h.GHArgv() != "" {
				t.Errorf("launch ran gh with pr_reviewer set:\n%s", h.GHArgv())
			}
			warned := strings.Contains(res.Stderr, "warning: no PR reviewer") && strings.Contains(res.Stderr, "set pr_reviewer")
			if warned != tc.wantWarning {
				t.Errorf("reviewer warning printed = %v, want %v\nstderr:\n%s", warned, tc.wantWarning, res.Stderr)
			}

			h.WaitForClaudeStart(30 * time.Second)
			sysPrompt := h.WorkerSystemPrompt()
			if tc.wantReviewer != "" {
				if want := "Use --reviewer " + tc.wantReviewer + " when creating the PR."; !strings.Contains(sysPrompt, want) {
					t.Errorf("system prompt missing %q:\n%s", want, sysPrompt)
				}
			} else if strings.Contains(sysPrompt, "--reviewer") {
				t.Errorf("system prompt should not request a reviewer:\n%s", sysPrompt)
			}

			h.ReleaseClaude()
			h.WaitForState(h.RunIDs()[0], func(s *run.State) bool { return s.TmuxPane == nil && s.Worktree == "" }, 30*time.Second)
		})
	}
}

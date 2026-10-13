//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// TestProvenanceTrailersAcrossPRFix drives two real launches: a fresh run
// commits through its worktree's hook and opens PR #4242 (per the stub log),
// then a --pr run on that PR inherits the change ID and amends the commit.
// The commit ends up with one Change-Id and both runs' Klaus-Run trailers.
func TestProvenanceTrailersAcrossPRFix(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	res := h.RunKlaus("launch", "add a health check endpoint")
	if res.ExitCode != 0 {
		t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	first, err := h.ReadState(h.RunIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	if !run.ValidChangeID(first.ChangeID) {
		t.Fatalf("fresh run's change_id = %q, want a new jj-form ID", first.ChangeID)
	}
	if !strings.Contains(res.Stdout, "change:   "+first.ChangeID+" (new)") {
		t.Errorf("launch output should name the new change ID:\n%s", res.Stdout)
	}

	// The agent commits and pushes; the hook stamps the commit.
	h.WaitForClaudeStart(30 * time.Second)
	if err := os.WriteFile(filepath.Join(first.Worktree, "health.go"), []byte("package health\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.git(first.Worktree, "add", "-A")
	h.git(first.Worktree, "commit", "-qm", "Add a health check")
	h.git(first.Worktree, "push", "-q", "origin", first.Branch)
	wantFirst := "Change-Id: " + first.ChangeID + "\nKlaus-Run: " + first.ID
	if got := h.GitOutput(first.Worktree, "log", "-1", "--format=%(trailers:only,unfold)"); got != wantFirst {
		t.Errorf("first commit trailers = %q, want %q", got, wantFirst)
	}
	if shared := h.GitOutput(h.RepoDir, "config", "--local", "--list"); strings.Contains(shared, "core.hookspath=") {
		t.Errorf("shared config should have no core.hooksPath:\n%s", shared)
	}

	h.ReleaseClaude()
	h.WaitForState(first.ID, func(s *run.State) bool { return s.TmuxPane == nil && s.Worktree == "" }, 30*time.Second)

	// gh answers for PR #4242, whose head is the first run's branch.
	h.WriteStub("gh", fmt.Sprintf(`#!/usr/bin/env bash
case "$*" in
  *headRefName*) echo %q ;;
  *"--json url"*) echo https://github.com/acme/widget/pull/4242 ;;
esac
exit 0
`, first.Branch))
	for _, f := range []string{"claude.release", "claude.started"} {
		os.Remove(filepath.Join(h.E2EDir, f))
	}

	res = h.RunKlaus("launch", "--pr", "4242", "address review")
	if res.ExitCode != 0 {
		t.Fatalf("launch --pr exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var fix *run.State
	for _, id := range h.RunIDs() {
		if id != first.ID {
			fix, _ = h.ReadState(id)
		}
	}
	if fix == nil {
		t.Fatalf("no --pr run recorded: %v", h.RunIDs())
	}
	if fix.ChangeID != first.ChangeID {
		t.Fatalf("--pr run change_id = %q, want the opener's %q\nstdout:\n%s", fix.ChangeID, first.ChangeID, res.Stdout)
	}

	h.WaitForClaudeStart(30 * time.Second)
	h.git(fix.Worktree, "commit", "-q", "--amend", "--no-edit")
	want := wantFirst + "\nKlaus-Run: " + fix.ID
	if got := h.GitOutput(fix.Worktree, "log", "-1", "--format=%(trailers:only,unfold)"); got != want {
		t.Errorf("amended commit trailers = %q, want %q", got, want)
	}

	h.ReleaseClaude()
	h.WaitForState(fix.ID, func(s *run.State) bool { return s.TmuxPane == nil }, 30*time.Second)
}

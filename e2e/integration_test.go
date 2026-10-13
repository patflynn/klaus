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

// pushBranch commits a file on a new branch off main in the sandbox repo and
// pushes it, as a direct-push agent finishes, then returns to main. It
// returns the pushed SHA.
func (h *Harness) pushBranch(branch, file string) string {
	h.t.Helper()
	h.git(h.RepoDir, "checkout", "-q", "-b", branch, "origin/main")
	if err := os.WriteFile(filepath.Join(h.RepoDir, file), []byte(branch+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.git(h.RepoDir, "add", file)
	h.git(h.RepoDir, "commit", "-qm", "add "+file)
	h.git(h.RepoDir, "push", "-q", "origin", branch)
	sha := h.GitOutput(h.RepoDir, "rev-parse", "HEAD")
	h.git(h.RepoDir, "checkout", "-q", "main")
	h.git(h.RepoDir, "branch", "-q", "-D", branch)
	return sha
}

// integrateBranch lands branch as the valley's integrator does when it can't
// fast-forward: one new commit on main whose tree is the merge-tree.
func (h *Harness) integrateBranch(branch string) {
	h.t.Helper()
	h.git(h.RepoDir, "fetch", "-q", "origin")
	main := h.GitOutput(h.RepoDir, "rev-parse", "origin/main")
	tree := h.GitOutput(h.RepoDir, "merge-tree", "--write-tree", main, "origin/"+branch)
	commit := h.GitOutput(h.RepoDir, "commit-tree", tree, "-p", main, "-m", "land "+branch)
	h.git(h.RepoDir, "push", "-q", "origin", commit+":refs/heads/main")
}

// seedPushed seeds a run that finished by pushing branch at sha from the
// sandbox repo's clone.
func (h *Harness) seedPushed(id, prompt, branch, sha string, created time.Time) {
	h.t.Helper()
	clone := h.RepoDir
	cost := 1.0
	h.SeedState(&run.State{
		ID: id, Type: "launch", Prompt: prompt, TargetRepo: strPtrE2E("acme/widget"), CloneDir: &clone,
		Branch: branch, PushedBranch: &branch, PushedSHA: &sha, CostUSD: &cost,
		CreatedAt: created.Format(time.RFC3339),
	})
}

func strPtrE2E(s string) *string { return &s }

// statusLine returns runID's row in `klaus status` output.
func statusLine(t *testing.T, out, runID string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 7 && f[0] == runID {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", runID, out)
	return ""
}

// statusOf returns the STATUS column of runID's row in `klaus status` output.
func statusOf(t *testing.T, out, runID string) string {
	t.Helper()
	return strings.Fields(statusLine(t, out, runID))[1]
}

// changeColumn returns the CHANGE column of runID's row in `klaus status`
// output: its PR's number, its valley handle, or its change ID's prefix.
func changeColumn(t *testing.T, out, runID string) string {
	t.Helper()
	return strings.Fields(statusLine(t, out, runID))[7]
}

// TestStatusShowsIntegration runs the real `klaus status` over pushed runs
// in every state, checked against the run's real clone and origin, and a PR
// that GitHub no longer shows the agent.
func TestStatusShowsIntegration(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	created := time.Now().Add(-time.Hour)

	waitingSHA := h.pushBranch("agent/waiting", "w.txt")
	h.seedPushed("20261012-0900-0001", "waiting", "agent/waiting", waitingSHA, created)

	ffSHA := h.pushBranch("agent/ff", "f.txt")
	h.git(h.RepoDir, "push", "-q", "origin", ffSHA+":refs/heads/main")
	h.seedPushed("20261012-0900-0002", "landed by fast-forward", "agent/ff", ffSHA, created)

	contentSHA := h.pushBranch("agent/content", "c.txt")
	h.integrateBranch("agent/content")
	h.git(h.RepoDir, "push", "-q", "origin", "--delete", "agent/content")
	h.seedPushed("20261012-0900-0003", "landed by the integrator", "agent/content", contentSHA, created)

	goneSHA := h.pushBranch("agent/gone", "g.txt")
	h.git(h.RepoDir, "push", "-q", "origin", "--delete", "agent/gone")
	h.seedPushed("20261012-0900-0004", "rejected", "agent/gone", goneSHA, created)

	// The clone's origin URL is broken: the check can't run, so it stays
	// awaiting rather than guessing.
	brokenClone := filepath.Join(t.TempDir(), "broken")
	h.git("", "clone", "-q", h.OriginDir, brokenClone)
	h.git(brokenClone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	cost := 1.0
	h.SeedState(&run.State{
		ID: "20261012-0900-0005", Type: "launch", Prompt: "unreachable", CloneDir: &brokenClone,
		PushedBranch: strPtrE2E("agent/gone"), PushedSHA: &goneSHA, CostUSD: &cost, CreatedAt: created.Format(time.RFC3339),
	})

	// GitHub answers every PR query for the old cosmo PR with "can't see it".
	h.WriteStub("gh", fmt.Sprintf(`#!/usr/bin/env bash
{ printf 'gh'; for a in "$@"; do printf ' %%q' "$a"; done; printf '\n'; } >> %q
echo "GraphQL: Could not resolve to a Repository with the name 'acme/cosmo'. (repository)" >&2
exit 1
`, filepath.Join(h.E2EDir, "gh.argv")))
	h.SeedState(&run.State{
		ID: "20261005-0900-0006", Type: "launch", Prompt: "old cosmo PR", CostUSD: &cost,
		PRURL: strPtrE2E("https://github.com/acme/cosmo/pull/7"), CreatedAt: created.Format(time.RFC3339),
	})

	res := h.RunKlaus("status")
	if res.ExitCode != 0 {
		t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	for id, want := range map[string]string{
		"20261012-0900-0001": "awaiting-integration",
		"20261012-0900-0002": "integrated",
		"20261012-0900-0003": "integrated",
		"20261012-0900-0004": "branch-gone",
		"20261012-0900-0005": "awaiting-integration",
		"20261005-0900-0006": "no-access",
	} {
		if got := statusOf(t, res.Stdout, id); got != want {
			t.Errorf("%s: status %q, want %q", id, got, want)
		}
	}
	// No CI, conflict or review queries for a PR we can't read.
	if calls := strings.TrimSpace(h.GHArgv()); strings.Count(calls, "\n") != 0 || !strings.Contains(calls, "state") {
		t.Errorf("gh should be asked only for the PR's state, got:\n%s", calls)
	}
}

// TestDashboardShowsWhatsYours runs the real dashboard over a pushed branch
// that is waiting and one that has landed: the waiting one is the operator's
// SUBMIT row, with the configured submit command in the footer; the landed
// one is done.
func TestDashboardShowsWhatsYours(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.AmendRepoConfig(map[string]any{
		"dashboard": map[string]any{"submit_command": "cd ~/hack/{repo} && valley review {branch}"},
	})

	landedSHA := h.pushBranch("agent/landed", "l.txt")
	h.git(h.RepoDir, "push", "-q", "origin", landedSHA+":refs/heads/main")
	h.seedPushed("20261012-0900-0001", "landed 01", "agent/landed", landedSHA, time.Now().Add(-20*time.Minute))
	waitingSHA := h.pushBranch("agent/waiting", "w.txt")
	h.seedPushed("20261012-0900-0002", "waiting 01", "agent/waiting", waitingSHA, time.Now().Add(-3*time.Hour))

	pane := strings.TrimSpace(h.tmux("new-session", "-d", "-s", "dash", "-x", "160", "-y", "14", "-c", h.RepoDir,
		"-P", "-F", "#{pane_id}", "env KLAUS_SESSION_ID="+h.SessionID+" klaus dashboard"))
	screen := func(want ...string) string {
		t.Helper()
		var last string
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			last = h.tmux("capture-pane", "-p", "-t", pane)
			ok := true
			for _, w := range want {
				ok = ok && strings.Contains(last, w)
			}
			if ok {
				return last
			}
		}
		t.Fatalf("dashboard screen never showed %q; last screen:\n%s", want, last)
		return ""
	}

	// The first check runs at startup.
	s := screen("1 yours", "1 done", "INTEGRATED")
	var rows []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "landed 01") || strings.Contains(line, "waiting 01") {
			rows = append(rows, line)
		}
	}
	if len(rows) != 2 || !strings.Contains(rows[0], "waiting 01") || !strings.Contains(rows[0], "SUBMIT") ||
		!strings.Contains(rows[1], "landed 01") {
		t.Errorf("want the SUBMIT row first, then the integrated one; screen:\n%s", s)
	}

	h.tmux("send-keys", "-t", pane, "g")
	screen("submit: cd ~/hack/widget && valley review agent/waiting")
}

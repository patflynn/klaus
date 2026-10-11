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

// launchOne runs `klaus launch args...`, which must succeed and record exactly
// one new run, and returns that run's state.
func launchOne(t *testing.T, h *Harness, args ...string) *run.State {
	t.Helper()
	before := h.RunIDs()
	res := h.RunKlaus(append([]string{"launch"}, args...)...)
	if res.ExitCode != 0 {
		t.Fatalf("launch %v exited %d\nstdout:\n%s\nstderr:\n%s", args, res.ExitCode, res.Stdout, res.Stderr)
	}
	ids := h.RunIDs()
	for _, id := range ids {
		if !contains(before, id) {
			st, err := h.ReadState(id)
			if err != nil {
				t.Fatalf("reading state: %v", err)
			}
			return st
		}
	}
	t.Fatalf("launch %v recorded no new run (runs: %v)", args, ids)
	return nil
}

// runIDTail is the random end of a run ID, the suffix of a derived branch name.
func runIDTail(id string) string {
	return id[strings.LastIndex(id, "-")+1:]
}

// TestLaunchDerivedBranchSalvaged launches from a brief that opens with a
// "# <repo>: title" heading. The worktree's branch is named from the title,
// and when the worker leaves unfinished work without a PR, finalize salvages
// it to origin under that same name.
func TestLaunchDerivedBranchSalvaged(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.01,"duration_ms":10,"session_id":"11111111-2222-4333-8444-555555555555"}` + "\n")
	h.WriteStub("claude", h.claudeStubScript()+"echo wip > unfinished.txt\n")

	brief := filepath.Join(h.E2EDir, "brief.md")
	if err := os.WriteFile(brief, []byte("# repo: Add a health check endpoint\n\nServe 200 on /healthz.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := launchOne(t, h, "--prompt-file", brief)

	want := "add-a-health-check-endpoint-" + runIDTail(st.ID)
	if st.Branch != want {
		t.Fatalf("state branch = %q, want %q", st.Branch, want)
	}
	if got := h.GitOutput(st.Worktree, "symbolic-ref", "--short", "HEAD"); got != want {
		t.Fatalf("worktree is on %q, want %q", got, want)
	}

	h.WaitForClaudeStart(30 * time.Second)
	releaseBackendWorkers(t, h)
	final, err := h.ReadState(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.NeedsAttention == nil {
		t.Fatalf("salvaged run not marked needs-attention: %+v", final)
	}
	if got := h.GitOutput(h.OriginDir, "show", want+":unfinished.txt"); got != "wip" {
		t.Fatalf("origin's %s holds unfinished.txt = %q, want the salvaged work", want, got)
	}
}

// TestLaunchBranchFlag covers the issue title as the name's source, --branch
// naming the branch exactly, and the --branch values launch refuses.
func TestLaunchBranchFlag(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteStub("gh", fmt.Sprintf(`#!/usr/bin/env bash
dir=%q
{ printf 'gh'; for a in "$@"; do printf ' %%q' "$a"; done; printf '\n'; } >> "$dir/gh.argv"
if [ "$1 $2" = "issue view" ]; then echo "Dashboard crashes on resize"; fi
exit 0
`, h.E2EDir))

	st := launchOne(t, h, "--issue", "7", "# Something else\n\nfix it")
	if want := "dashboard-crashes-on-resize-" + runIDTail(st.ID); st.Branch != want {
		t.Errorf("issue launch branch = %q, want %q", st.Branch, want)
	}
	if !strings.Contains(h.GHArgv(), "gh issue view") {
		t.Errorf("launch did not look up the issue title; gh calls:\n%s", h.GHArgv())
	}

	st = launchOne(t, h, "--branch", "feat/explicit-name", "--issue", "7", "# Something else")
	if st.Branch != "feat/explicit-name" {
		t.Errorf("--branch launch state branch = %q, want feat/explicit-name", st.Branch)
	}
	if got := h.GitOutput(st.Worktree, "symbolic-ref", "--short", "HEAD"); got != "feat/explicit-name" {
		t.Errorf("--branch worktree is on %q, want feat/explicit-name", got)
	}

	h.git(h.RepoDir, "push", "-q", "origin", "main:refs/heads/taken-on-origin")
	runs := h.RunIDs()
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"with --pr", "mutually exclusive", []string{"--branch", "x", "--pr", "1"}},
		{"invalid name", "not a valid branch name", []string{"--branch", "bad..name"}},
		{"empty name", "not a valid branch name", []string{"--branch", ""}},
		{"existing local branch", "already exists", []string{"--branch", "main"}},
		{"branch only on origin", "already exists on origin", []string{"--branch", "taken-on-origin"}},
		{"branch of a live run", "already exists", []string{"--branch", "feat/explicit-name"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := h.RunKlaus(append([]string{"launch"}, append(tc.args, "do it")...)...)
			if res.ExitCode == 0 {
				t.Fatalf("launch %v succeeded, want a refusal\nstdout:\n%s", tc.args, res.Stdout)
			}
			if !strings.Contains(res.Stderr, tc.want) {
				t.Errorf("stderr should say %q, got:\n%s", tc.want, res.Stderr)
			}
		})
	}
	if got := h.RunIDs(); len(got) != len(runs) {
		t.Errorf("refused launches recorded runs: %v -> %v", runs, got)
	}

	h.WaitForClaudeStart(30 * time.Second)
	releaseBackendWorkers(t, h)
}

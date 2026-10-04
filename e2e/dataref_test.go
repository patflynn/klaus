//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

const dataRef = "refs/klaus/data"

// refSHA returns the sha ref resolves to in the git dir, or "" if absent.
func (h *Harness) refSHA(gitDir, ref string) string {
	h.t.Helper()
	cmd := exec.Command("git", "--git-dir", gitDir, "rev-parse", "--verify", "--quiet", ref)
	cmd.Env = append(os.Environ(), "HOME="+h.Home, "GIT_CONFIG_GLOBAL="+filepath.Join(h.Home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// launchAndFinalize launches an agent, lets the fake claude finish, and waits
// for _finalize to complete (cost recorded, pane released). It returns the
// run id.
func (h *Harness) launchAndFinalize(prompt string) string {
	h.t.Helper()
	res := h.RunKlaus("launch", prompt)
	if res.ExitCode != 0 {
		h.t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	ids := h.RunIDs()
	if len(ids) != 1 {
		h.t.Fatalf("expected 1 run, got %d: %v", len(ids), ids)
	}
	h.WaitForClaudeStart(30 * time.Second)
	h.ReleaseClaude()
	h.WaitForState(ids[0], func(s *run.State) bool {
		return s.CostUSD != nil && s.TmuxPane == nil
	}, 30*time.Second)
	return ids[0]
}

// TestDataRefStaysLocalByDefault: with the default config, finalize commits
// the run to the sandbox repo's data ref and origin never receives it.
// push-log publishes it only when given --push.
func TestDataRefStaysLocalByDefault(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	localGitDir := filepath.Join(h.RepoDir, ".git")

	runID := h.launchAndFinalize("add a health check endpoint")

	if h.refSHA(localGitDir, dataRef+":runs/"+runID+".json") == "" {
		t.Fatalf("local %s has no runs/%s.json after finalize", dataRef, runID)
	}
	if sha := h.refSHA(h.OriginDir, dataRef); sha != "" {
		t.Fatalf("origin has %s at %s after finalize; the default config must not push it", dataRef, sha)
	}

	res := h.RunKlaus("push-log", runID)
	if res.ExitCode != 0 {
		t.Fatalf("push-log exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Not pushed") {
		t.Errorf("push-log should say it did not push; stdout:\n%s", res.Stdout)
	}
	if sha := h.refSHA(h.OriginDir, dataRef); sha != "" {
		t.Fatalf("push-log without --push published %s", dataRef)
	}

	res = h.RunKlaus("push-log", "--push", runID)
	if res.ExitCode != 0 {
		t.Fatalf("push-log --push exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Pushed "+dataRef+" to origin") {
		t.Errorf("push-log --push should report the push; stdout:\n%s", res.Stdout)
	}
	local := h.refSHA(localGitDir, dataRef)
	if got := h.refSHA(h.OriginDir, dataRef); got == "" || got != local {
		t.Errorf("origin %s = %q after push-log --push, want %s", dataRef, got, local)
	}
}

// TestDataRefPushedWhenEnabled: push_data_ref: true makes finalize push the
// data ref to origin. It is set globally because a host-repo run's finalize
// reads the repo config from the agent's worktree, which holds the committed
// copy.
func TestDataRefPushedWhenEnabled(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	if err := os.MkdirAll(filepath.Join(h.Home, ".klaus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Home, ".klaus", "config.json"), []byte(`{"push_data_ref": true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runID := h.launchAndFinalize("add a health check endpoint")

	local := h.refSHA(filepath.Join(h.RepoDir, ".git"), dataRef)
	if local == "" {
		t.Fatalf("local %s missing after finalize of %s", dataRef, runID)
	}
	if got := h.refSHA(h.OriginDir, dataRef); got != local {
		t.Errorf("origin %s = %q, want the local commit %s", dataRef, got, local)
	}
}

//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/valley"
)

// useRealValley has the harness use the valley CLI on PATH, and skips the
// test unless there is one whose help lists the verbs a change's life
// takes: ready, unready and changes. It sets valley.principal in the
// sandbox repo, which ready needs to name the change's owner; every
// worktree reads that repo's config.
func (h *Harness) useRealValley() {
	h.t.Helper()
	path, err := exec.LookPath("valley")
	if err != nil {
		h.t.Skip("no valley on PATH")
	}
	help, _ := exec.Command(path, "help").CombinedOutput()
	for _, verb := range []string{"ready", "unready", "changes"} {
		if !regexp.MustCompile(`(?m)^\s*` + verb + `\b`).Match(help) {
			h.t.Skipf("the valley on PATH (%s) has no %s", path, verb)
		}
	}
	h.git(h.RepoDir, "config", "valley.principal", "klaus-env")
}

// valleyChanges is the real valley's view of the sandbox repo, as `valley
// changes --json` prints it.
func (h *Harness) valleyChanges() valley.Repo {
	h.t.Helper()
	cmd := exec.Command("valley", "changes", "--json")
	cmd.Dir, cmd.Env = h.RepoDir, h.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("valley changes: %v: %s", err, stderr.String())
	}
	l, err := valley.Decode(out)
	if err != nil || len(l.Repos) != 1 || l.Repos[0].Err() != "" {
		h.t.Fatalf("valley changes printed %s (%v)", out, err)
	}
	return l.Repos[0]
}

// gitTry runs real git in dir as GitOutput does, returning its combined
// output and error instead of failing the test.
func (h *Harness) gitTry(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+h.Home, "GIT_CONFIG_GLOBAL="+filepath.Join(h.Home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// leasedPush returns the push command the worker prompt gives for a
// restack, split into git's arguments.
func leasedPush(t *testing.T, sys string) []string {
	t.Helper()
	m := regexp.MustCompile("(?m)^`git (push --force-with-lease=[^`]+)`$").FindStringSubmatch(sys)
	if m == nil {
		t.Fatalf("worker prompt gives no leased push:\n%s", sys)
	}
	return strings.Fields(m[1])
}

// TestChangeRestacksOntoMain drives a valley change through a conflict with
// main, with real launches against the real valley CLI and bare repos
// (issue #346). A first run pushes v1 and finalize readies it; main then
// takes a commit that conflicts with it. A --change run is told it may
// restack the change and how to push it: it rebases onto origin/main,
// resolves the conflict and pushes with the prompt's own command, leased on
// the head it started from. Finalize records the rewritten head as v2 of
// the same change on the same branch, and origin holds no other branch. A
// later --change by the full change ID, as a conflict dispatch names the
// change, resolves to v2, and its lease, on v2, refuses to overwrite a push
// someone made meanwhile; finalize doesn't overwrite it either.
func TestChangeRestacksOntoMain(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.useRealValley()
	h.joinValley()
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")

	first, _ := h.launchRun("add a greeting")
	h.WaitForClaudeStart(30 * time.Second)
	v1 := h.pushWork(first, "greeting.txt", "Add a greeting")
	first = h.finishRun(first.ID)
	if first.Valley == nil || first.Valley.Version != 1 || first.Valley.Error != "" || first.Valley.Handle == "" {
		t.Fatalf("finalize should mark v1 ready: %+v", first.Valley)
	}
	branch, handle := first.Branch, first.Valley.Handle

	// Main takes a commit that conflicts with the change.
	if err := os.WriteFile(filepath.Join(h.RepoDir, "greeting.txt"), []byte("Greet from main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.git(h.RepoDir, "add", "greeting.txt")
	h.git(h.RepoDir, "commit", "-qm", "Greet from main")
	h.git(h.RepoDir, "push", "-q", "origin", "main")
	mainHead := h.GitOutput(h.RepoDir, "rev-parse", "HEAD")

	// --change <handle>: the worker is told how to restack the change.
	h.resetWorker()
	restack, res := h.launchRun("--change", handle, "rebase the change onto main and resolve the conflict")
	h.WaitForClaudeStart(30 * time.Second)
	for _, want := range []string{
		"(continuing change " + handle + ")",
		"branch:   " + branch + "\n",
		"change:   " + first.ChangeID + " (inherited from change " + handle + ")",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("launch output missing %q:\n%s", want, res.Stdout)
		}
	}
	sys := h.WorkerSystemPrompt()
	if strings.Contains(sys, "do not rebase") || !strings.Contains(sys, "never merge another branch into it") {
		t.Errorf("a valley change's worker should be told to rebase, never merge:\n%s", sys)
	}
	push := leasedPush(t, sys)
	if want := "push --force-with-lease=refs/heads/" + branch + ":" + v1 + " origin HEAD:refs/heads/" + branch; strings.Join(push, " ") != want {
		t.Errorf("leased push = %q, want %q", strings.Join(push, " "), want)
	}

	// The worker rebases, resolves the conflict and pushes as told.
	wt := restack.Worktree
	h.git(wt, "fetch", "-q", "origin")
	if out, err := h.gitTry(wt, "rebase", "origin/main"); err == nil || !strings.Contains(out, "CONFLICT") {
		t.Fatalf("rebasing onto main should conflict: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(wt, "greeting.txt"), []byte("Greet from main\nAdd a greeting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.git(wt, "add", "greeting.txt")
	h.git(wt, "-c", "core.editor=true", "rebase", "--continue")
	h.git(wt, push...)
	v2 := h.GitOutput(wt, "rev-parse", "HEAD")
	if tip := h.GitOutput(h.OriginDir, "rev-parse", "refs/heads/"+branch); tip != v2 {
		t.Fatalf("origin's %s at %s, want the rebased %s", branch, tip, v2)
	}
	h.git(h.OriginDir, "merge-base", "--is-ancestor", mainHead, v2)

	// Finalize records the rewritten head as v2 of the same change.
	restack = h.finishRun(restack.ID)
	if restack.ChangeID != first.ChangeID || restack.PushedBranch == nil || *restack.PushedBranch != branch ||
		restack.PushedSHA == nil || *restack.PushedSHA != v2 || restack.NeedsAttention != nil {
		t.Errorf("the restack run recorded change %s, push %v at %v, attention %v; want %s, %s at %s, none",
			restack.ChangeID, restack.PushedBranch, restack.PushedSHA, restack.NeedsAttention, first.ChangeID, branch, v2)
	}
	if v := restack.Valley; v == nil || v.Handle != handle || v.Version != 2 || v.Error != "" {
		t.Errorf("finalize should mark the rebased head ready as %s v2: %+v", handle, v)
	}
	repo := h.valleyChanges()
	if len(repo.Changes) != 1 {
		t.Fatalf("the valley should record one change, got %+v", repo.Changes)
	}
	ch := repo.Changes[0]
	if ch.ChangeID != first.ChangeID || ch.State != valley.StateReady || ch.Active != 2 || len(ch.Versions) != 2 ||
		ch.Versions[0].Head != v1 || ch.Versions[1].Head != v2 || ch.Versions[1].Branch != branch {
		t.Errorf("the valley should record v1 at %s and v2 at %s, on %s, of %s: %+v", v1, v2, branch, first.ChangeID, ch)
	}
	if len(repo.Branches) != 0 {
		t.Errorf("the valley lists stray branches: %+v", repo.Branches)
	}
	if got := h.originBranches(); strings.Join(got, " ") != branch+" main" {
		t.Errorf("origin's branches = %v, want only %s and main", got, branch)
	}

	// A later --change by the full change ID starts from v2 and leases on it.
	h.resetWorker()
	later, res := h.launchRun("--change", first.ChangeID, "tidy the greeting")
	h.WaitForClaudeStart(30 * time.Second)
	if later.Branch != branch || later.ChangeID != first.ChangeID || strings.Contains(res.Stderr, "warning") {
		t.Errorf("later run on %s, change %s, stderr %q; want %s, %s and no warning", later.Branch, later.ChangeID, res.Stderr, branch, first.ChangeID)
	}
	if head := h.GitOutput(later.Worktree, "rev-parse", "HEAD"); head != v2 {
		t.Errorf("later run's worktree at %s, want v2 %s", head, v2)
	}
	push = leasedPush(t, h.WorkerSystemPrompt())
	if lease := "--force-with-lease=refs/heads/" + branch + ":" + v2; push[1] != lease {
		t.Errorf("later run's lease %q, want %q", push[1], lease)
	}

	// Someone else pushes to the branch meanwhile: the lease refuses to
	// overwrite it, and so does finalize.
	other := filepath.Join(t.TempDir(), "other")
	h.git("", "clone", "-q", "-b", branch, h.OriginDir, other)
	if err := os.WriteFile(filepath.Join(other, "farewell.txt"), []byte("Bye\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.git(other, "add", "farewell.txt")
	h.git(other, "commit", "-qm", "Add a farewell")
	h.git(other, "push", "-q", "origin", branch)
	theirs := h.GitOutput(other, "rev-parse", "HEAD")
	h.git(later.Worktree, "commit", "-q", "--amend", "-m", "Add a tidier greeting")
	if out, err := h.gitTry(later.Worktree, push...); err == nil || !strings.Contains(out, "stale info") {
		t.Errorf("the leased push should be refused as stale: %v\n%s", err, out)
	}
	h.finishRun(later.ID)
	if tip := h.GitOutput(h.OriginDir, "rev-parse", "refs/heads/"+branch); tip != theirs {
		t.Errorf("origin's %s at %s, want the concurrent push %s kept", branch, tip, theirs)
	}
	if got := h.originBranches(); strings.Join(got, " ") != branch+" main" {
		t.Errorf("origin's branches = %v, want only %s and main", got, branch)
	}
}

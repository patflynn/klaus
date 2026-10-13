package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testChangeID = "zyxwvutsrqponmlkzyxwvutsrqponmlk"
	testRunA     = "20261013-1200-aaaaaaaa"
	testRunB     = "20261013-1300-bbbbbbbb"
)

// isolateGitConfig points git at an empty system config and a temp global
// config holding only a committer identity, so the host's hooksPath or
// trailer settings cannot leak into the tests.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[user]\n\tname = Test\n\temail = test@test.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// testClone creates a bare origin with one commit on main and returns a clone
// of it, the way klaus clones target repos.
func testClone(t *testing.T) string {
	t.Helper()
	isolateGitConfig(t)
	seed := initTestRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	clone := filepath.Join(t.TempDir(), "clone")
	mustGit(t, "", "init", "--quiet", "--bare", "--initial-branch=main", origin)
	mustGit(t, seed, "push", "--quiet", origin, "main")
	mustGit(t, "", "clone", "--quiet", origin, clone)
	return clone
}

// addWorktree adds a linked worktree of clone on a new branch and returns it.
func addWorktree(t *testing.T, clone, name string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), name)
	if err := WorktreeAdd(context.Background(), clone, wt, name, "main"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	return wt
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile writes a file in dir and commits it with msg, running hooks.
func commitFile(t *testing.T, dir, name, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", name)
	mustGit(t, dir, "commit", "--quiet", "-m", msg)
}

func headMessage(t *testing.T, dir string) string {
	t.Helper()
	return mustGit(t, dir, "log", "-1", "--format=%B")
}

// trailerValues returns the values of key in HEAD's trailer block, in order.
func trailerValues(t *testing.T, dir, key string) []string {
	t.Helper()
	out := mustGit(t, dir, "log", "-1", "--format=%(trailers:key="+key+",valueonly,separator=%x0A)")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func sharedHooksPaths(t *testing.T, clone string) []string {
	t.Helper()
	out, err := runGit(context.Background(), clone, "config", "--local", "--get-all", "core.hooksPath")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func install(t *testing.T, wt string, prov Provenance) {
	t.Helper()
	if err := InstallCommitMsgHook(context.Background(), wt, prov); err != nil {
		t.Fatalf("InstallCommitMsgHook: %v", err)
	}
}

func TestInstallCommitMsgHook_ScopedToWorktree(t *testing.T) {
	clone := testClone(t)
	wtA := addWorktree(t, clone, "run-a")
	wtB := addWorktree(t, clone, "run-b")
	install(t, wtA, Provenance{RunID: testRunA, ChangeID: testChangeID})
	install(t, wtB, Provenance{RunID: testRunB, ChangeID: "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"})

	if got := sharedHooksPaths(t, clone); len(got) != 0 {
		t.Errorf("shared config has core.hooksPath %v, want none", got)
	}

	commitFile(t, wtA, "a.txt", "change in a")
	commitFile(t, wtB, "b.txt", "change in b")
	if got := trailerValues(t, wtA, "Klaus-Run"); len(got) != 1 || got[0] != testRunA {
		t.Errorf("worktree a Klaus-Run = %v, want [%s]\n%s", got, testRunA, headMessage(t, wtA))
	}
	if got := trailerValues(t, wtB, "Klaus-Run"); len(got) != 1 || got[0] != testRunB {
		t.Errorf("worktree b Klaus-Run = %v, want [%s]\n%s", got, testRunB, headMessage(t, wtB))
	}

	// The main clone runs no klaus hook, before and after a worktree goes.
	commitFile(t, clone, "main.txt", "change in main")
	if msg := headMessage(t, clone); msg != "change in main" {
		t.Errorf("main clone commit was rewritten by a hook:\n%s", msg)
	}
	if err := WorktreeRemove(context.Background(), clone, wtA); err != nil {
		t.Fatal(err)
	}
	commitFile(t, wtB, "b2.txt", "another change in b")
	if got := trailerValues(t, wtB, "Klaus-Run"); len(got) != 1 || got[0] != testRunB {
		t.Errorf("worktree b lost its hook after a removed: Klaus-Run = %v", got)
	}
}

func TestInstallCommitMsgHook_RepairsLeakedSharedHooksPath(t *testing.T) {
	t.Run("value inside a worktree's git dir is removed", func(t *testing.T) {
		clone := testClone(t)
		gone := addWorktree(t, clone, "gone")
		goneHooks := filepath.Join(clone, ".git", "worktrees", "gone", "hooks")
		mustGit(t, clone, "config", "core.hooksPath", goneHooks)
		if err := WorktreeRemove(context.Background(), clone, gone); err != nil {
			t.Fatal(err)
		}

		install(t, addWorktree(t, clone, "run-a"), Provenance{RunID: testRunA})
		if got := sharedHooksPaths(t, clone); len(got) != 0 {
			t.Errorf("shared core.hooksPath = %v, want the leaked value removed", got)
		}
	})

	t.Run("value elsewhere is kept", func(t *testing.T) {
		clone := testClone(t)
		own := filepath.Join(clone, ".git", "hooks")
		mustGit(t, clone, "config", "core.hooksPath", own)

		install(t, addWorktree(t, clone, "run-a"), Provenance{RunID: testRunA})
		if got := sharedHooksPaths(t, clone); len(got) != 1 || got[0] != own {
			t.Errorf("shared core.hooksPath = %v, want [%s] kept", got, own)
		}
	})
}

// holdConfigLock takes the clone's config.lock, as a concurrent git config
// does. After delay it writes the shared config with the git config args in
// update applied into the lock and renames the lock into place, the way git
// commits a config change. The returned channel reports how that went.
func holdConfigLock(t *testing.T, clone string, delay time.Duration, update ...string) <-chan error {
	t.Helper()
	config := filepath.Join(clone, ".git", "config")
	lock := config + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	scratch := filepath.Join(t.TempDir(), "config")
	released := make(chan error, 1)
	go func() {
		time.Sleep(delay)
		released <- func() error {
			old, err := os.ReadFile(config)
			if err != nil {
				return err
			}
			if err := os.WriteFile(scratch, old, 0o644); err != nil {
				return err
			}
			args := append([]string{"config", "--file", scratch}, update...)
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("git %v: %v\n%s", args, err, out)
			}
			next, err := os.ReadFile(scratch)
			if err != nil {
				return err
			}
			if err := os.WriteFile(lock, next, 0o644); err != nil {
				return err
			}
			return os.Rename(lock, config)
		}()
	}()
	return released
}

// TestInstallCommitMsgHook_WaitsForConcurrentConfigWrite covers two launches
// installing at once: the one that loses config.lock must wait for the winner
// to put the same change in place, not fail on seeing the old config.
func TestInstallCommitMsgHook_WaitsForConcurrentConfigWrite(t *testing.T) {
	t.Run("enabling extensions.worktreeConfig", func(t *testing.T) {
		clone := testClone(t)
		wt := addWorktree(t, clone, "run-a")
		released := holdConfigLock(t, clone, 300*time.Millisecond, "extensions.worktreeConfig", "true")

		install(t, wt, Provenance{RunID: testRunA})
		if err := <-released; err != nil {
			t.Fatalf("releasing config.lock: %v", err)
		}
		commitFile(t, wt, "a.txt", "change in a")
		if got := trailerValues(t, wt, "Klaus-Run"); len(got) != 1 || got[0] != testRunA {
			t.Errorf("Klaus-Run = %v, want [%s]", got, testRunA)
		}
	})

	t.Run("removing a leaked core.hooksPath", func(t *testing.T) {
		clone := testClone(t)
		leakedHooks := filepath.Join(clone, ".git", "worktrees", "gone", "hooks")
		mustGit(t, clone, "config", "core.hooksPath", leakedHooks)
		wt := addWorktree(t, clone, "run-a")
		released := holdConfigLock(t, clone, 300*time.Millisecond, "--unset-all", "core.hooksPath")

		install(t, wt, Provenance{RunID: testRunA})
		if err := <-released; err != nil {
			t.Fatalf("releasing config.lock: %v", err)
		}
		if got := sharedHooksPaths(t, clone); len(got) != 0 {
			t.Errorf("shared core.hooksPath = %v, want the leaked value removed", got)
		}
	})

	t.Run("a lock never released fails once the wait is over", func(t *testing.T) {
		defer func(d time.Duration) { configLockWait = d }(configLockWait)
		configLockWait = 200 * time.Millisecond
		clone := testClone(t)
		wt := addWorktree(t, clone, "run-a")
		lock := filepath.Join(clone, ".git", "config.lock")
		if err := os.WriteFile(lock, nil, 0o644); err != nil {
			t.Fatal(err)
		}

		err := InstallCommitMsgHook(context.Background(), wt, Provenance{RunID: testRunA})
		if err == nil || !strings.Contains(err.Error(), "extensions.worktreeConfig") {
			t.Errorf("InstallCommitMsgHook with a stale config.lock = %v, want an error enabling extensions.worktreeConfig", err)
		}
	})
}

// writeRepoHook installs a commit-msg hook in dir that appends a trailer and
// saves the message it was given to seen.
func writeRepoHook(t *testing.T, dir, seen string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncp \"$1\" '" + seen + "'\nprintf 'Repo-Hook: ran\\n' >> \"$1\"\n"
	if err := os.WriteFile(filepath.Join(dir, "commit-msg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInstallCommitMsgHook_ChainsRepositoryHook(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hooks func(clone string) string // where the repository's hook lives
	}{
		{"common hooks dir", func(clone string) string { return filepath.Join(clone, ".git", "hooks") }},
		{"shared core.hooksPath", func(clone string) string {
			dir := filepath.Join(filepath.Dir(clone), "repo-hooks")
			mustGit(t, clone, "config", "core.hooksPath", dir)
			return dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clone := testClone(t)
			seen := filepath.Join(t.TempDir(), "seen")
			writeRepoHook(t, tc.hooks(clone), seen)
			wt := addWorktree(t, clone, "run-a")
			install(t, wt, Provenance{RunID: testRunA, ChangeID: testChangeID})

			commitFile(t, wt, "a.txt", "change\n\nCo-Authored-By: Claude <noreply@anthropic.com>")

			got, err := os.ReadFile(seen)
			if err != nil {
				t.Fatalf("repository hook did not run: %v", err)
			}
			if !strings.Contains(string(got), "Klaus-Run: "+testRunA) || strings.Contains(string(got), "Claude") {
				t.Errorf("repository hook should run after klaus's, but saw:\n%s", got)
			}
			if msg := headMessage(t, wt); !strings.HasSuffix(msg, "Repo-Hook: ran") {
				t.Errorf("commit should carry the repository hook's edit:\n%s", msg)
			}
		})
	}
}

func TestInstallCommitMsgHook_ProvenanceTrailers(t *testing.T) {
	clone := testClone(t)
	wt := addWorktree(t, clone, "run-a")
	install(t, wt, Provenance{RunID: testRunA, ChangeID: testChangeID})

	commitFile(t, wt, "a.txt", "add a\n\nBody text.")
	want := "add a\n\nBody text.\n\nChange-Id: " + testChangeID + "\nKlaus-Run: " + testRunA
	if msg := headMessage(t, wt); msg != want {
		t.Errorf("message =\n%s\nwant\n%s", msg, want)
	}

	// A later run on the same change amends the commit.
	install(t, wt, Provenance{RunID: testRunB, ChangeID: testChangeID})
	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("amended\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "commit", "--quiet", "-a", "--amend", "--no-edit")
	if got := trailerValues(t, wt, "Change-Id"); len(got) != 1 || got[0] != testChangeID {
		t.Errorf("Change-Id = %v, want one %s", got, testChangeID)
	}
	if got := trailerValues(t, wt, "Klaus-Run"); len(got) != 2 || got[0] != testRunA || got[1] != testRunB {
		t.Errorf("Klaus-Run = %v, want [%s %s]", got, testRunA, testRunB)
	}

	// The same run amending again adds nothing.
	mustGit(t, wt, "commit", "--quiet", "--amend", "--no-edit")
	if got := trailerValues(t, wt, "Klaus-Run"); len(got) != 2 {
		t.Errorf("Klaus-Run = %v after a same-run amend, want 2 entries", got)
	}

	// An empty message still aborts the commit instead of becoming trailers.
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "")
	cmd.Dir = wt
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("empty-message commit should abort, got success:\n%s\n%s", out, headMessage(t, wt))
	}
}

// TestInstallCommitMsgHook_AttributionAndTrailers checks attribution stripping
// and the trailers together: the stripped lines go, a human co-author stays in
// the trailer block, and the provenance trailers join it.
func TestInstallCommitMsgHook_AttributionAndTrailers(t *testing.T) {
	clone := testClone(t)
	wt := addWorktree(t, clone, "run-a")
	install(t, wt, Provenance{RunID: testRunA, ChangeID: testChangeID})

	commitFile(t, wt, "a.txt", "fix bug\n\n"+
		"Co-Authored-By: Claude Opus 4 <noreply@anthropic.com>\n"+
		"Co-Authored-By: Alice Smith <alice@example.com>\n"+
		"🤖 Generated with Claude Code")

	want := "fix bug\n\nCo-Authored-By: Alice Smith <alice@example.com>\nChange-Id: " + testChangeID + "\nKlaus-Run: " + testRunA
	if msg := headMessage(t, wt); msg != want {
		t.Errorf("message =\n%s\nwant\n%s", msg, want)
	}
}

func TestInstallCommitMsgHook_RejectsMainWorktree(t *testing.T) {
	clone := testClone(t)
	if err := InstallCommitMsgHook(context.Background(), clone, Provenance{}); err == nil {
		t.Fatal("installing in the main worktree should fail: its hooks dir is the repository's own")
	}
}

func TestCommitChangeID(t *testing.T) {
	clone := testClone(t)
	wt := addWorktree(t, clone, "run-a")
	ctx := context.Background()
	if id, err := CommitChangeID(ctx, wt, "HEAD"); err != nil || id != "" {
		t.Errorf("CommitChangeID on an unstamped commit = %q, %v; want empty", id, err)
	}
	install(t, wt, Provenance{RunID: testRunA, ChangeID: testChangeID})
	commitFile(t, wt, "a.txt", "add a")
	if id, err := CommitChangeID(ctx, wt, "HEAD"); err != nil || id != testChangeID {
		t.Errorf("CommitChangeID = %q, %v; want %s", id, err, testChangeID)
	}
}

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
)

const testDataRef = "refs/klaus/data"

// finalizePRLog is a clean run that opened a PR, so _finalize takes the
// normal path (no salvage) through the data-ref sync.
const finalizePRLog = `{"type":"assistant","message":{"content":[{"type":"text","text":"Opened https://github.com/acme/widget/pull/9"}]}}
{"type":"result","subtype":"success","total_cost_usd":1,"duration_ms":1000}
`

// TestFinalizeKeepsDataRefLocalByDefault runs the real _finalize against a
// clone with a bare origin: the run lands on the local data ref, and origin
// never receives it.
func TestFinalizeKeepsDataRefLocalByDefault(t *testing.T) {
	origin, repo, worktree, branch := setupBareRemote(t)
	commitAndPush(t, worktree, branch)

	_, state, _ := finalizeRealRepo(t, repo, worktree, branch, finalizePRLog, nil)

	files, err := gitOut(t, repo, "ls-tree", "-r", "--name-only", testDataRef)
	if err != nil {
		t.Fatalf("local %s missing after finalize: %v %s", testDataRef, err, files)
	}
	for _, want := range []string{"runs/" + state.ID + ".json", "logs/" + state.ID + ".jsonl"} {
		if !strings.Contains(files, want) {
			t.Errorf("local %s missing %s; has:\n%s", testDataRef, want, files)
		}
	}
	if sha, err := gitOut(t, origin, "rev-parse", "--verify", "--quiet", testDataRef); err == nil {
		t.Fatalf("origin has %s at %s; the default config must not push it", testDataRef, sha)
	}
}

// TestFinalizePushesDataRefWhenEnabled: push_data_ref: true restores the push.
func TestFinalizePushesDataRefWhenEnabled(t *testing.T) {
	origin, repo, worktree, branch := setupBareRemote(t)
	commitAndPush(t, worktree, branch)
	if err := os.MkdirAll(filepath.Join(repo, ".klaus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".klaus", "config.json"), []byte(`{"push_data_ref": true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	finalizeRealRepo(t, repo, worktree, branch, finalizePRLog, nil)

	local, err := gitOut(t, repo, "rev-parse", testDataRef)
	if err != nil {
		t.Fatalf("local %s missing after finalize: %v", testDataRef, err)
	}
	if remote, err := gitOut(t, origin, "rev-parse", "--verify", testDataRef); err != nil || remote != local {
		t.Fatalf("origin %s = %q (%v), want the local commit %s", testDataRef, remote, err, local)
	}
}

// finishedClaudeRun saves a finished run whose log and resumable conversation
// exist on disk under home, as _finalize sees one.
func finishedClaudeRun(t *testing.T, store run.StateStore, home, id, sessionID string) *run.State {
	t.Helper()
	worktree := filepath.Join(home, "worktrees", id)
	conv := claudeConversationPath(worktree, sessionID)
	if err := os.MkdirAll(filepath.Dir(conv), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conv, []byte(makeConversationJSONL(sessionID, worktree)), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(store.LogDir(), id+".jsonl")
	if err := os.WriteFile(logFile, []byte(`{"type":"result","subtype":"success"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sid := sessionID
	st := &run.State{
		ID:              id,
		Branch:          "feature-x",
		Worktree:        worktree,
		LogFile:         &logFile,
		CreatedAt:       "2026-10-04T" + id[9:11] + ":" + id[11:13] + ":00Z", // id is YYYYMMDD-HHMM-xxxx
		ClaudeSessionID: &sid,
	}
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return st
}

// TestReplayReadsUnpushedDataRef: trajectory replay needs nothing on origin.
// It reads the local data ref finalize committed to, and its best-effort
// fetch neither fails the replay nor rewinds a local ref that is ahead of a
// copy published before push_data_ref existed.
func TestReplayReadsUnpushedDataRef(t *testing.T) {
	setup := func(t *testing.T) (home, origin, repo string, store run.StateStore) {
		home = t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		origin, repo, _, _ = setupBareRemote(t)
		s := run.NewHomeDirStoreFromPath(filepath.Join(t.TempDir(), "session"))
		if err := s.EnsureDirs(); err != nil {
			t.Fatalf("EnsureDirs: %v", err)
		}
		return home, origin, repo, s
	}
	replay := func(store run.StateStore, repo, home string) replayDecision {
		return resolveBudgetPausedReplay(context.Background(), replayParams{
			GitClient:   git.NewExecClient(),
			Store:       store,
			RepoRoot:    repo,
			DataRef:     testDataRef,
			Worktree:    filepath.Join(home, "worktrees", "resumed"),
			PRBranch:    "feature-x",
			PRNumber:    "7",
			ForceReplay: true, // skip the gh paused-label check
			ThresholdKB: 300,
		})
	}
	ctx := context.Background()

	t.Run("origin never had the ref", func(t *testing.T) {
		home, origin, repo, store := setup(t)
		const sessionID = "6623bdcf-1dce-4da0-86b3-a46743d208e8"
		st := finishedClaudeRun(t, store, home, "20261004-1000-aaaa", sessionID)
		syncRunToDataRef(ctx, repo, store, git.NewExecClient(), testDataRef, false, st)

		if d := replay(store, repo, home); d.SessionUUID != sessionID {
			t.Fatalf("SessionUUID = %q, want %q (%s)", d.SessionUUID, sessionID, d.Reason)
		}
		if _, err := gitOut(t, origin, "rev-parse", "--verify", "--quiet", testDataRef); err == nil {
			t.Errorf("origin gained %s", testDataRef)
		}
	})

	t.Run("origin holds a stale published ref", func(t *testing.T) {
		home, origin, repo, store := setup(t)
		gc := git.NewExecClient()
		old := finishedClaudeRun(t, store, home, "20261004-0900-aaaa", "11111111-2222-4333-8444-555555555555")
		syncRunToDataRef(ctx, repo, store, gc, testDataRef, true, old)
		published, err := gitOut(t, origin, "rev-parse", "--verify", testDataRef)
		if err != nil {
			t.Fatalf("setup: first run not published: %v", err)
		}

		const sessionID = "99999999-8888-4777-8666-555555555555"
		newer := finishedClaudeRun(t, store, home, "20261004-1000-bbbb", sessionID)
		syncRunToDataRef(ctx, repo, store, gc, testDataRef, false, newer)
		local, _ := gitOut(t, repo, "rev-parse", testDataRef)

		d := replay(store, repo, home)
		if d.SessionUUID != sessionID || d.SourceRunID != newer.ID {
			t.Fatalf("resumed %q from %q, want %q from %q (%s)", d.SessionUUID, d.SourceRunID, sessionID, newer.ID, d.Reason)
		}
		if got, _ := gitOut(t, repo, "rev-parse", testDataRef); got != local {
			t.Errorf("local %s moved from %s to %s during replay's fetch", testDataRef, local, got)
		}
		if got, _ := gitOut(t, origin, "rev-parse", testDataRef); got != published {
			t.Errorf("origin %s = %q, want it left at the published %s", testDataRef, got, published)
		}
	})
}

// TestPushLogPushesOnlyWhenAsked: push-log commits a held-back log to the
// local data ref, and publishes it only with --push.
func TestPushLogPushesOnlyWhenAsked(t *testing.T) {
	origin, repo, _, _ := setupBareRemote(t)
	t.Setenv("HOME", t.TempDir())
	const sessionID = "20261004-1100-pushlog-session"
	t.Setenv(sessionIDEnv, sessionID)
	store, err := run.NewHomeDirStore(sessionID)
	if err != nil {
		t.Fatalf("NewHomeDirStore: %v", err)
	}
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	const id = "20261004-1100-aaaa"
	logFile := filepath.Join(store.LogDir(), id+".jsonl")
	if err := os.WriteFile(logFile, []byte(`{"type":"result","subtype":"success"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&run.State{ID: id, Branch: "b", CreatedAt: "2026-10-04T11:00:00Z", LogFile: &logFile, CloneDir: &repo}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Cleanup(func() { pushLogPush = false })
	pushLog := func(push bool) error {
		pushLogPush = push
		pushLogCmd.SetContext(context.Background())
		return pushLogCmd.RunE(pushLogCmd, []string{id})
	}

	if err := pushLog(false); err != nil {
		t.Fatalf("push-log: %v", err)
	}
	if files, _ := gitOut(t, repo, "ls-tree", "-r", "--name-only", testDataRef); !strings.Contains(files, "logs/"+id+".jsonl") {
		t.Fatalf("local %s missing the log; has:\n%s", testDataRef, files)
	}
	if _, err := gitOut(t, origin, "rev-parse", "--verify", "--quiet", testDataRef); err == nil {
		t.Fatalf("push-log without --push published %s", testDataRef)
	}

	if err := pushLog(true); err != nil {
		t.Fatalf("push-log --push: %v", err)
	}
	local, _ := gitOut(t, repo, "rev-parse", testDataRef)
	if remote, err := gitOut(t, origin, "rev-parse", "--verify", testDataRef); err != nil || remote != local {
		t.Fatalf("origin %s = %q (%v), want %s", testDataRef, remote, err, local)
	}

	runGitCmd(t, repo, "remote", "set-url", "origin", origin+"-gone")
	if err := pushLog(true); err == nil {
		t.Error("push-log --push must fail when the push fails")
	}
}

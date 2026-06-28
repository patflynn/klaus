package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/draft"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
)

// initReplayTestRepo creates a real git repo with an initial commit.
func initReplayTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	steps := [][]string{
		{"git", "init", "--initial-branch=main", dir},
		{"git", "-C", dir, "config", "user.email", "test@test.com"},
		{"git", "-C", dir, "config", "user.name", "Test"},
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	steps = append(steps,
		[]string{"git", "-C", dir, "add", "README.md"},
		[]string{"git", "-C", dir, "commit", "-m", "initial"},
	)
	for _, args := range steps {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// makeConversationJSONL builds a small conversation-format trajectory (the
// schema claude --resume reads) carrying the given sessionId and cwd.
func makeConversationJSONL(sessionID, cwd string) string {
	lines := []string{
		fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":%q,"version":"2.1.158","gitBranch":"feature-x","uuid":"11111111-1111-1111-1111-111111111111","parentUuid":null,"isSidechain":false,"message":{"role":"user","content":"do the thing"}}`, cwd, sessionID),
		fmt.Sprintf(`{"type":"assistant","cwd":%q,"sessionId":%q,"version":"2.1.158","uuid":"22222222-2222-2222-2222-222222222222","parentUuid":"11111111-1111-1111-1111-111111111111","message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]}}`, cwd, sessionID),
	}
	return strings.Join(lines, "\n") + "\n"
}

// storeTrajectoryOnDataRef writes a conversation trajectory onto refs/klaus/data
// at sessions/<runID>.jsonl using the real data-ref machinery.
func storeTrajectoryOnDataRef(t *testing.T, repo, dataRef, runID, content string) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "traj.jsonl")
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"sessions/" + runID + ".jsonl": tmp}
	if err := git.SyncToDataRef(context.Background(), repo, dataRef, "Run "+runID, files); err != nil {
		t.Fatalf("SyncToDataRef: %v", err)
	}
}

// TestReplayRoundTripEndToEnd exercises the full replay path against a REAL
// refs/klaus/data blob: store a conversation trajectory, then resolve a
// budget-paused replay and assert klaus restores the JSONL into the new
// worktree's project dir and that the downstream claude command resumes it.
func TestReplayRoundTripEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir checks USERPROFILE first on Windows

	repo := initReplayTestRepo(t)
	const dataRef = "refs/klaus/data"
	const runID = "20260601-1000-aaaa"
	const branch = "feature-x"
	const sessionID = "6623bdcf-1dce-4da0-86b3-a46743d208e8"

	// New worktree the resumed agent will run in. The trajectory's original
	// cwd is intentionally different to mimic the cross-worktree handoff.
	worktree := filepath.Join(home, "worktrees", runID+"-resume")
	origCwd := filepath.Join(home, "worktrees", runID)
	traj := makeConversationJSONL(sessionID, origCwd)
	storeTrajectoryOnDataRef(t, repo, dataRef, runID, traj)

	// Record the paused run in the store, keyed to the PR branch.
	baseDir := filepath.Join(home, ".klaus", "sessions", "session-test")
	store := run.NewHomeDirStoreFromPath(baseDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	sid := sessionID
	if err := store.Save(&run.State{
		ID:              runID,
		Branch:          branch,
		Prompt:          "do the thing",
		CreatedAt:       "2026-06-01T10:00:00Z",
		ClaudeSessionID: &sid,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	decision := resolveBudgetPausedReplay(context.Background(), replayParams{
		GitClient:   git.NewExecClient(),
		Store:       store,
		RepoRoot:    repo,
		DataRef:     dataRef,
		Worktree:    worktree,
		PRBranch:    branch,
		PRNumber:    "42",
		ForceReplay: true, // skip the gh paused-label check in this unit
		ThresholdKB: 300,
	})

	if decision.SessionUUID != sessionID {
		t.Fatalf("SessionUUID = %q, want %q (reason: %s)", decision.SessionUUID, sessionID, decision.Reason)
	}
	if decision.SourceRunID != runID {
		t.Errorf("SourceRunID = %q, want %q", decision.SourceRunID, runID)
	}

	// The trajectory must be restored into the NEW worktree's project dir,
	// byte-for-byte, under <sessionID>.jsonl — exactly where claude --resume
	// looks when invoked from that worktree.
	dest := claudeConversationPath(worktree, sessionID)
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("restored trajectory not found at %s: %v", dest, err)
	}
	if string(got) != traj {
		t.Errorf("restored trajectory mismatch:\n got %q\nwant %q", got, traj)
	}

	// The downstream claude command resumes the restored session.
	cmd := buildClaudeCommand("sys", "5", "continue", "20260601-1100-bbbb", decision.SessionUUID)
	if !strings.Contains(cmd, "--resume '"+sessionID+"'") {
		t.Errorf("claude command missing --resume %s: %s", sessionID, cmd)
	}
	if !strings.Contains(cmd, "--fork-session") {
		t.Errorf("claude command missing --fork-session: %s", cmd)
	}
}

// TestReplayFallbacks covers the miss paths that must drop back to a fresh
// agent (empty SessionUUID with an explanatory reason).
func TestReplayFallbacks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir checks USERPROFILE first on Windows
	repo := initReplayTestRepo(t)
	const dataRef = "refs/klaus/data"
	const branch = "feature-x"
	worktree := filepath.Join(home, "wt")

	newStore := func() run.StateStore {
		baseDir := filepath.Join(t.TempDir(), "session")
		s := run.NewHomeDirStoreFromPath(baseDir)
		if err := s.EnsureDirs(); err != nil {
			t.Fatalf("EnsureDirs: %v", err)
		}
		return s
	}

	t.Run("no prior run for branch", func(t *testing.T) {
		d := resolveBudgetPausedReplay(context.Background(), replayParams{
			GitClient: git.NewExecClient(), Store: newStore(), RepoRoot: repo,
			DataRef: dataRef, Worktree: worktree, PRBranch: branch, PRNumber: "1",
			ForceReplay: true, ThresholdKB: 300,
		})
		if d.SessionUUID != "" {
			t.Errorf("expected fresh fallback, got resume %q", d.SessionUUID)
		}
	})

	t.Run("trajectory missing from data ref", func(t *testing.T) {
		store := newStore()
		_ = store.Save(&run.State{ID: "20260601-1000-cccc", Branch: branch, CreatedAt: "2026-06-01T10:00:00Z"})
		d := resolveBudgetPausedReplay(context.Background(), replayParams{
			GitClient: git.NewExecClient(), Store: store, RepoRoot: repo,
			DataRef: dataRef, Worktree: worktree, PRBranch: branch, PRNumber: "1",
			ForceReplay: true, ThresholdKB: 300,
		})
		if d.SessionUUID != "" {
			t.Errorf("expected fresh fallback when blob absent, got %q (%s)", d.SessionUUID, d.Reason)
		}
	})

	t.Run("oversized trajectory falls back unless forced", func(t *testing.T) {
		const runID = "20260601-1000-dddd"
		const sessionID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		store := newStore()
		_ = store.Save(&run.State{ID: runID, Branch: branch, CreatedAt: "2026-06-01T10:00:00Z"})

		// Build a trajectory larger than a 1KB threshold.
		big := makeConversationJSONL(sessionID, filepath.Join(home, "orig")) + strings.Repeat("x", 4096) + "\n"
		storeTrajectoryOnDataRef(t, repo, dataRef, runID, big)

		// Paused PR confirmed via a fake runner so the gate passes; threshold
		// then forces the fallback because --replay is not set.
		orig := budgetPauseRunner
		budgetPauseRunner = fakeLabelRunner{labels: "klaus:budget-paused\n"}
		defer func() { budgetPauseRunner = orig }()

		d := resolveBudgetPausedReplay(context.Background(), replayParams{
			GitClient: git.NewExecClient(), Store: store, RepoRoot: repo,
			DataRef: dataRef, Worktree: worktree, PRBranch: branch, PRNumber: "1",
			ForceReplay: false, ThresholdKB: 1,
		})
		if d.SessionUUID != "" {
			t.Errorf("expected fresh fallback for oversized trajectory, got %q", d.SessionUUID)
		}
		if !strings.Contains(d.Reason, "threshold") {
			t.Errorf("reason should mention threshold, got %q", d.Reason)
		}

		// --replay (ForceReplay) bypasses the size threshold.
		d2 := resolveBudgetPausedReplay(context.Background(), replayParams{
			GitClient: git.NewExecClient(), Store: store, RepoRoot: repo,
			DataRef: dataRef, Worktree: worktree, PRBranch: branch, PRNumber: "1",
			ForceReplay: true, ThresholdKB: 1,
		})
		if d2.SessionUUID != sessionID {
			t.Errorf("--replay should bypass threshold and resume, got %q (%s)", d2.SessionUUID, d2.Reason)
		}
	})

	t.Run("not paused falls back without forcing", func(t *testing.T) {
		const runID = "20260601-1000-eeee"
		const sessionID = "ffffffff-0000-1111-2222-333333333333"
		store := newStore()
		_ = store.Save(&run.State{ID: runID, Branch: branch, CreatedAt: "2026-06-01T10:00:00Z"})
		storeTrajectoryOnDataRef(t, repo, dataRef, runID, makeConversationJSONL(sessionID, home))

		orig := budgetPauseRunner
		budgetPauseRunner = fakeLabelRunner{labels: "some-other-label\n"} // not paused
		defer func() { budgetPauseRunner = orig }()

		d := resolveBudgetPausedReplay(context.Background(), replayParams{
			GitClient: git.NewExecClient(), Store: store, RepoRoot: repo,
			DataRef: dataRef, Worktree: worktree, PRBranch: branch, PRNumber: "1",
			ForceReplay: false, ThresholdKB: 300,
		})
		if d.SessionUUID != "" {
			t.Errorf("expected fresh fallback when PR not paused, got %q", d.SessionUUID)
		}
	})
}

func TestEncodeProjectPath(t *testing.T) {
	// Claude replaces both '/' and '.' with '-'. These cases are derived from
	// real ~/.claude/projects/ directory names on this machine.
	tests := []struct {
		cwd  string
		want string
	}{
		{"/tmp/klaus-sessions/k/2026.06", "-tmp-klaus-sessions-k-2026-06"},
		{
			"/tmp/klaus-sessions/klaus/20260627-1721-bcde1ba8",
			"-tmp-klaus-sessions-klaus-20260627-1721-bcde1ba8",
		},
		{
			// '/.klaus' becomes '--klaus' because both '/' and '.' map to '-'.
			"/home/patrick/.klaus/sessions/abc-workspace",
			"-home-patrick--klaus-sessions-abc-workspace",
		},
	}
	for _, tt := range tests {
		if got := encodeProjectPath(tt.cwd); got != tt.want {
			t.Errorf("encodeProjectPath(%q) = %q, want %q", tt.cwd, got, tt.want)
		}
	}
}

func TestExtractConversationSessionID(t *testing.T) {
	blob := makeConversationJSONL("9a9a9a9a-0000-0000-0000-000000000000", "/x")
	if got := extractConversationSessionID([]byte(blob)); got != "9a9a9a9a-0000-0000-0000-000000000000" {
		t.Errorf("extractConversationSessionID = %q", got)
	}
	if got := extractConversationSessionID([]byte("not json\n{}\n")); got != "" {
		t.Errorf("expected empty for blob without sessionId, got %q", got)
	}
}

// fakeLabelRunner is a draft.Runner that returns canned gh output for the
// label lookup and is a no-op for git.
type fakeLabelRunner struct {
	labels string
	err    error
}

func (f fakeLabelRunner) Git(ctx context.Context, workdir string, args ...string) (string, error) {
	return "", nil
}

func (f fakeLabelRunner) GH(ctx context.Context, workdir string, args ...string) (string, error) {
	return f.labels, f.err
}

var _ draft.Runner = fakeLabelRunner{}

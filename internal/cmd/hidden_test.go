package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
)

func TestFinalizeWorktreeCleanup(t *testing.T) {
	t.Run("clears worktree from state after cleanup", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "runs")
		if err := os.MkdirAll(stateDir, 0755); err != nil {
			t.Fatal(err)
		}

		// Create a real git repo with a worktree so we can verify removal.
		repoDir := filepath.Join(dir, "repo")
		initGitRepo(t, repoDir)

		wtPath := filepath.Join(dir, "wt")
		runGitCmd(t, repoDir, "worktree", "add", wtPath, "-b", "agent/test-branch")

		// Verify worktree exists before cleanup.
		if _, err := os.Stat(wtPath); err != nil {
			t.Fatalf("worktree should exist before cleanup: %v", err)
		}

		state := &run.State{
			ID:       "test-run",
			Branch:   "agent/test-branch",
			Worktree: wtPath,
			CloneDir: &repoDir,
		}
		store := &testStateStore{dir: stateDir, state: state}

		// Simulate the cleanup logic from _finalize.
		cleanupWorktree(context.Background(), store, git.NewExecClient(), state, false)

		if state.Worktree != "" {
			t.Errorf("expected Worktree to be cleared, got %q", state.Worktree)
		}
		if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
			t.Error("expected worktree directory to be removed")
		}
	})

	t.Run("idempotent when worktree already removed", func(t *testing.T) {
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "runs")
		if err := os.MkdirAll(stateDir, 0755); err != nil {
			t.Fatal(err)
		}

		repoDir := filepath.Join(dir, "repo")
		initGitRepo(t, repoDir)

		state := &run.State{
			ID:       "test-run",
			Branch:   "agent/gone-branch",
			Worktree: filepath.Join(dir, "already-gone"),
			CloneDir: &repoDir,
		}
		store := &testStateStore{dir: stateDir, state: state}

		// Should not panic or fail — just clears state.
		cleanupWorktree(context.Background(), store, git.NewExecClient(), state, false)

		if state.Worktree != "" {
			t.Errorf("expected Worktree to be cleared, got %q", state.Worktree)
		}
	})

	t.Run("keeps a branch with unpushed commits", func(t *testing.T) {
		_, repo, worktree, branch := setupBareRemote(t)
		runGitCmd(t, worktree, "add", "-A")
		runGitCmd(t, worktree, "commit", "-m", "local only")
		state := &run.State{ID: "test-run", Branch: branch, Worktree: worktree, CloneDir: &repo}
		store := &testStateStore{dir: t.TempDir(), state: state}

		cleanupWorktree(context.Background(), store, git.NewExecClient(), state, false)

		if _, err := gitOut(t, repo, "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
			t.Errorf("unpushed branch was deleted: %v", err)
		}
	})

	t.Run("keeps a branch whose origin copy was deleted", func(t *testing.T) {
		origin, repo, worktree, branch := setupBareRemote(t)
		commitAndPush(t, worktree, branch)
		runGitCmd(t, origin, "branch", "-D", branch) // no fetch: origin/<branch> is stale
		state := &run.State{ID: "test-run", Branch: branch, Worktree: worktree, CloneDir: &repo}
		store := &testStateStore{dir: t.TempDir(), state: state}

		cleanupWorktree(context.Background(), store, git.NewExecClient(), state, false)

		if _, err := gitOut(t, repo, "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
			t.Errorf("branch deleted on a stale origin ref: %v", err)
		}
	})

	t.Run("no-op when worktree field is empty", func(t *testing.T) {
		state := &run.State{ID: "test-run", Worktree: ""}
		store := &testStateStore{state: state}

		cleanupWorktree(context.Background(), store, git.NewExecClient(), state, false)

		if state.Worktree != "" {
			t.Errorf("expected empty worktree, got %q", state.Worktree)
		}
	})
}

// initGitRepo creates a bare-minimum git repo with one commit.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, dir, "init")
	runGitCmd(t, dir, "commit", "--allow-empty", "-m", "init")
}

// repoWithOrigin creates an empty git repo whose origin is origin.
func repoWithOrigin(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	runGitCmd(t, dir, "init", "-q")
	runGitCmd(t, dir, "remote", "add", "origin", origin)
	return dir
}

// runGitCmd runs a git command in the given directory.
func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func TestKillAgentPane(t *testing.T) {
	paneID := "%42"

	t.Run("kills pane and clears state", func(t *testing.T) {
		state := &run.State{ID: "test-run", TmuxPane: &paneID}
		store := &testStateStore{state: state}
		tc := &fakeTmux{killedPanes: []string{}}

		killAgentPane(context.Background(), store, tc, state)

		if len(tc.killedPanes) != 1 || tc.killedPanes[0] != paneID {
			t.Errorf("expected KillPane(%q), got %v", paneID, tc.killedPanes)
		}
		if state.TmuxPane != nil {
			t.Errorf("expected TmuxPane to be nil, got %v", *state.TmuxPane)
		}
	})

	t.Run("no-op when TmuxPane is nil", func(t *testing.T) {
		state := &run.State{ID: "test-run", TmuxPane: nil}
		store := &testStateStore{state: state}
		tc := &fakeTmux{}

		killAgentPane(context.Background(), store, tc, state)

		if len(tc.killedPanes) != 0 {
			t.Errorf("expected no KillPane calls, got %v", tc.killedPanes)
		}
	})
}

func TestExtractPRNumberFromURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "standard PR URL",
			url:  "https://github.com/owner/repo/pull/123",
			want: "123",
		},
		{
			name: "PR URL with trailing slash",
			url:  "https://github.com/owner/repo/pull/456/",
			want: "456",
		},
		{
			name: "PR URL with query params",
			url:  "https://github.com/owner/repo/pull/789?diff=split",
			want: "789",
		},
		{
			name: "PR URL with fragment",
			url:  "https://github.com/owner/repo/pull/42#discussion_r123",
			want: "42",
		},
		{
			name: "PR URL with files path",
			url:  "https://github.com/owner/repo/pull/99/files",
			want: "99",
		},
		{
			name: "not a PR URL",
			url:  "https://github.com/owner/repo/issues/123",
			want: "",
		},
		{
			name: "empty string",
			url:  "",
			want: "",
		},
		{
			name: "no number after pull",
			url:  "https://github.com/owner/repo/pull/",
			want: "",
		},
		{
			name: "large PR number",
			url:  "https://github.com/owner/repo/pull/12345",
			want: "12345",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPRNumberFromURL(tt.url)
			if got != tt.want {
				t.Errorf("extractPRNumberFromURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestExtractPRURL(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "URL in sentence",
			text: "I created a PR at https://github.com/owner/repo/pull/42 for review.",
			want: "https://github.com/owner/repo/pull/42",
		},
		{
			name: "URL with trailing period",
			text: "See https://github.com/owner/repo/pull/99.",
			want: "https://github.com/owner/repo/pull/99",
		},
		{
			name: "no PR URL",
			text: "This is just some text without a PR link.",
			want: "",
		},
		{
			name: "issue URL not matched",
			text: "Check https://github.com/owner/repo/issues/10 for details.",
			want: "",
		},
		{
			name: "empty text",
			text: "",
			want: "",
		},
		{
			name: "URL only",
			text: "https://github.com/owner/repo/pull/1",
			want: "https://github.com/owner/repo/pull/1",
		},
		{
			name: "markdown link",
			text: "Created [PR #42](https://github.com/owner/repo/pull/42) for review.",
			want: "https://github.com/owner/repo/pull/42",
		},
		{
			name: "angle brackets",
			text: "PR created: <https://github.com/owner/repo/pull/7>",
			want: "https://github.com/owner/repo/pull/7",
		},
		{
			name: "parenthesized URL",
			text: "See the PR (https://github.com/owner/repo/pull/55) for details.",
			want: "https://github.com/owner/repo/pull/55",
		},
		{
			name: "URL with trailing comma",
			text: "https://github.com/owner/repo/pull/10, which fixes the bug",
			want: "https://github.com/owner/repo/pull/10",
		},
		{
			name: "http URL",
			text: "http://github.com/owner/repo/pull/3",
			want: "http://github.com/owner/repo/pull/3",
		},
		{
			name: "bare gh pr create output",
			text: "https://github.com/owner/repo/pull/42\n",
			want: "https://github.com/owner/repo/pull/42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPRURL(tt.text)
			if got != tt.want {
				t.Errorf("extractPRURL(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestIsAllowedPRURL(t *testing.T) {
	httpsClone := repoWithOrigin(t, "https://github.com/acme/widget.git")
	sshClone := repoWithOrigin(t, "git@github.com:acme/widget.git")
	localClone := repoWithOrigin(t, "/srv/git/widget.git")
	slugTarget := "acme/widget"
	otherTarget := "acme/corp"
	bareTarget := "widget"
	// launch records a local-path origin as a path, not owner/repo.
	pathTarget := "/srv/git/widget"
	const widgetPR = "https://github.com/acme/widget/pull/123"
	const foreignPR = "https://github.com/other/repo/pull/123"

	tests := []struct {
		name      string
		state     *run.State
		candidate string
		want      bool
	}{
		{
			name:      "owner/repo TargetRepo matches",
			state:     &run.State{TargetRepo: &slugTarget, CloneDir: &localClone},
			candidate: widgetPR,
			want:      true,
		},
		{
			name:      "owner/repo TargetRepo matches case-insensitively",
			state:     &run.State{TargetRepo: &slugTarget, CloneDir: &localClone},
			candidate: "https://github.com/ACME/Widget/pull/123",
			want:      true,
		},
		{
			name:      "owner/repo TargetRepo wins over clone origin",
			state:     &run.State{TargetRepo: &otherTarget, CloneDir: &httpsClone},
			candidate: widgetPR,
			want:      false,
		},
		{
			name:      "bare TargetRepo falls back to clone origin",
			state:     &run.State{TargetRepo: &bareTarget, CloneDir: &httpsClone},
			candidate: widgetPR,
			want:      true,
		},
		{
			name:      "bare TargetRepo rejects foreign repo",
			state:     &run.State{TargetRepo: &bareTarget, CloneDir: &httpsClone},
			candidate: foreignPR,
			want:      false,
		},
		{
			name:      "path TargetRepo falls back to clone origin",
			state:     &run.State{TargetRepo: &pathTarget, CloneDir: &httpsClone},
			candidate: foreignPR,
			want:      false,
		},
		{
			name:      "ssh origin matches",
			state:     &run.State{CloneDir: &sshClone},
			candidate: widgetPR,
			want:      true,
		},
		{
			name:      "ssh origin rejects foreign repo",
			state:     &run.State{CloneDir: &sshClone},
			candidate: foreignPR,
			want:      false,
		},
		{
			name:      "local-path origin accepts any real slug",
			state:     &run.State{TargetRepo: &pathTarget, CloneDir: &localClone},
			candidate: foreignPR,
			want:      true,
		},
		{
			name:      "local-path origin still rejects placeholder",
			state:     &run.State{TargetRepo: &pathTarget, CloneDir: &localClone},
			candidate: "https://github.com/owner/repo/pull/123",
			want:      false,
		},
		{
			name:      "rejects placeholder owner/repo",
			state:     &run.State{TargetRepo: &slugTarget},
			candidate: "https://github.com/owner/repo/pull/123",
			want:      false,
		},
		{
			name:      "rejects placeholder OWNER/REPO",
			state:     &run.State{TargetRepo: &slugTarget},
			candidate: "https://github.com/OWNER/REPO/pull/123",
			want:      false,
		},
		{
			name:      "rejects placeholder <owner>/<repo>",
			state:     &run.State{TargetRepo: &slugTarget},
			candidate: "https://github.com/<owner>/<repo>/pull/123",
			want:      false,
		},
		{
			name:      "rejects placeholder org/repo",
			state:     &run.State{TargetRepo: &slugTarget},
			candidate: "https://github.com/org/repo/pull/123",
			want:      false,
		},
		{
			name:      "rejects placeholder user/repo",
			state:     &run.State{TargetRepo: &slugTarget},
			candidate: "https://github.com/user/repo/pull/123",
			want:      false,
		},
		{
			name:      "empty candidate",
			state:     &run.State{TargetRepo: &slugTarget},
			candidate: "",
			want:      false,
		},
		{
			name:      "malformed candidate without repo",
			state:     &run.State{TargetRepo: &slugTarget},
			candidate: "https://github.com/pull/123",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAllowedPRURL(prURLTargetSlug(tt.state), tt.candidate)
			if got != tt.want {
				t.Errorf("isAllowedPRURL(%q) = %v, want %v", tt.candidate, got, tt.want)
			}
		})
	}
}

func TestFinalizeFromLog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
		suffix string
	}{
		{name: "session limit success with is_error"},
		{name: "session limit clears earlier failure", prefix: `{"type":"result","subtype":"error_during_execution","is_error":true}` + "\n"},
		{name: "session limit survives nonzero exit", suffix: "\n" + `{"type":"klaus_exit","exit_code":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const sessionID = "79dcfb08-4de4-45f4-b7db-056bccbc3a00"
			logContent := tc.prefix + `{"type":"result","subtype":"success","is_error":true,"session_id":"` + sessionID + `","total_cost_usd":1.2,"duration_ms":100}` + tc.suffix
			state, store := setupFinalizeTest(t, logContent)
			subtype, err := finalizeFromLog(store, state)
			if err != nil {
				t.Fatal(err)
			}
			if subtype != "success" || state.FailureReason != nil {
				t.Fatalf("subtype = %q, FailureReason = %v; want success without failure", subtype, state.FailureReason)
			}
			assertCost(t, state, 1.2)
			assertDuration(t, state, 100)
			if state.ClaudeSessionID == nil || *state.ClaudeSessionID != sessionID {
				t.Fatalf("ClaudeSessionID = %v, want %q", state.ClaudeSessionID, sessionID)
			}
		})
	}

	t.Run("extracts PR URL from assistant text", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5-20250929"}
{"type":"assistant","message":{"content":[{"type":"text","text":"I'll create the PR now."}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Created PR at https://github.com/patflynn/klaus/pull/42"}]}}
{"type":"result","total_cost_usd":1.5,"duration_ms":30000}
`
		state, store := setupFinalizeTest(t, logContent)
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/patflynn/klaus/pull/42")
		assertCost(t, state, 1.5)
		assertDuration(t, state, 30000)
	})

	t.Run("extracts PR URL from tool_result event", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5-20250929"}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"gh pr create --title test"}}]}}
{"type":"tool_result","content":"https://github.com/patflynn/klaus/pull/99\n"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Done! I created the PR."}]}}
{"type":"result","total_cost_usd":2.0,"duration_ms":45000}
`
		state, store := setupFinalizeTest(t, logContent)
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/patflynn/klaus/pull/99")
	})

	t.Run("extracts PR URL from user message with tool_result content", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5-20250929"}
{"type":"user","message":{"content":[{"type":"tool_result","content":"https://github.com/patflynn/klaus/pull/7\n"}]}}
{"type":"result","total_cost_usd":1.0,"duration_ms":10000}
`
		state, store := setupFinalizeTest(t, logContent)
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/patflynn/klaus/pull/7")
	})

	t.Run("handles markdown link in assistant text", func(t *testing.T) {
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"Created [PR #42](https://github.com/patflynn/klaus/pull/42) for review."}]}}
{"type":"result","total_cost_usd":1.0,"duration_ms":5000}
`
		state, store := setupFinalizeTest(t, logContent)
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/patflynn/klaus/pull/42")
	})

	t.Run("survives malformed JSONL lines", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init"}
not valid json at all
{"truncated":
{"type":"assistant","message":{"content":[{"type":"text","text":"PR: https://github.com/patflynn/klaus/pull/5"}]}}
{"type":"result","total_cost_usd":0.5,"duration_ms":2000}
`
		state, store := setupFinalizeTest(t, logContent)
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/patflynn/klaus/pull/5")
		assertCost(t, state, 0.5)
	})

	t.Run("no PR URL in log", func(t *testing.T) {
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"Just doing some work."}]}}
{"type":"result","total_cost_usd":0.1,"duration_ms":1000}
`
		state, store := setupFinalizeTest(t, logContent)
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		if state.PRURL != nil {
			t.Errorf("expected nil PRURL, got %q", *state.PRURL)
		}
	})

	t.Run("last PR URL wins", func(t *testing.T) {
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"First PR: https://github.com/patflynn/klaus/pull/1"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Recreated PR: https://github.com/patflynn/klaus/pull/2"}]}}
{"type":"result","total_cost_usd":1.0,"duration_ms":5000}
`
		state, store := setupFinalizeTest(t, logContent)
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/patflynn/klaus/pull/2")
	})

	t.Run("preserves PRURL set before finalization", func(t *testing.T) {
		// Simulates --pr mode: launch.go sets state.PRURL to the real PR
		// before the agent runs. The agent's tool output contains unrelated
		// PR URLs (e.g. from source code, test fixtures, or comments) that
		// would otherwise clobber the correct value.
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"Looking at https://github.com/other/repo/pull/999 for reference"}]}}
{"type":"tool_result","content":"see https://github.com/some/fixture/pull/123 in test data"}
{"type":"result","total_cost_usd":1.0,"duration_ms":5000}
`
		state, store := setupFinalizeTest(t, logContent)
		existing := "https://github.com/patflynn/klaus/pull/42"
		state.PRURL = &existing

		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/patflynn/klaus/pull/42")
	})

	t.Run("marks FailureReason on error_during_execution result", func(t *testing.T) {
		// Mirrors the real crash: a cross-worktree --resume that exits
		// instantly with no turns. _finalize must not treat this as success.
		logContent := `{"type":"system","subtype":"init"}
{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"total_cost_usd":0,"errors":["No conversation found with session ID: deadbeef"]}
`
		state, store := setupFinalizeTest(t, logContent)
		subtype, err := finalizeFromLog(store, state)
		if err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		if subtype != "error_during_execution" {
			t.Errorf("subtype = %q, want error_during_execution", subtype)
		}
		if state.FailureReason == nil {
			t.Fatal("expected FailureReason to be set, got nil")
		}
		if !strings.Contains(*state.FailureReason, "error_during_execution") {
			t.Errorf("FailureReason = %q, want it to mention the subtype", *state.FailureReason)
		}
		if !strings.Contains(*state.FailureReason, "No conversation found") {
			t.Errorf("FailureReason = %q, want it to include the first error", *state.FailureReason)
		}
		// A crashed run had no PR; PRURL must stay nil.
		if state.PRURL != nil {
			t.Errorf("expected nil PRURL on crash, got %q", *state.PRURL)
		}
	})

	t.Run("marks FailureReason when is_error is true without a subtype", func(t *testing.T) {
		logContent := `{"type":"result","is_error":true,"total_cost_usd":0.2,"duration_ms":1000}
`
		state, store := setupFinalizeTest(t, logContent)
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		if state.FailureReason == nil {
			t.Fatal("expected FailureReason to be set when is_error is true")
		}
	})

	t.Run("successful result leaves FailureReason nil and records cost", func(t *testing.T) {
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"Done: https://github.com/patflynn/klaus/pull/12"}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":7,"total_cost_usd":1.25,"duration_ms":20000}
`
		state, store := setupFinalizeTest(t, logContent)
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		if state.FailureReason != nil {
			t.Errorf("expected nil FailureReason on success, got %q", *state.FailureReason)
		}
		assertCost(t, state, 1.25)
		assertDuration(t, state, 20000)
		assertPRURL(t, state, "https://github.com/patflynn/klaus/pull/12")
	})

	t.Run("extracts PRURL from log when not set before finalization", func(t *testing.T) {
		// Simulates new-PR mode: state.PRURL is nil until the agent runs
		// `gh pr create`. Regex extraction fills it in from the log.
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"Creating PR now."}]}}
{"type":"tool_result","content":"https://github.com/patflynn/klaus/pull/77\n"}
{"type":"result","total_cost_usd":1.0,"duration_ms":5000}
`
		state, store := setupFinalizeTest(t, logContent)
		if state.PRURL != nil {
			t.Fatalf("test precondition: expected nil PRURL before finalize")
		}

		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/patflynn/klaus/pull/77")
	})

	t.Run("rejects placeholder PR URL in assistant text", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5-20250929"}
{"type":"assistant","message":{"content":[{"type":"text","text":"You can create a PR like https://github.com/owner/repo/pull/123 for review."}]}}
{"type":"result","total_cost_usd":0.5,"duration_ms":10000}
`
		state, store := setupFinalizeTest(t, logContent)
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		if state.PRURL != nil {
			t.Errorf("expected nil PRURL for placeholder URL, got %q", *state.PRURL)
		}
	})

	t.Run("prefers tool_result matching TargetRepo over assistant text from foreign repo", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5-20250929"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Check https://github.com/someone-else/other/pull/9 for example"}]}}
{"type":"tool_result","content":"https://github.com/patflynn/klaus/pull/310\n"}
{"type":"result","total_cost_usd":1.0,"duration_ms":15000}
`
		state, store := setupFinalizeTest(t, logContent)
		target := "patflynn/klaus"
		state.TargetRepo = &target
		if _, err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/patflynn/klaus/pull/310")
	})

	t.Run("rejects placeholders in tool_result", func(t *testing.T) {
		for _, placeholder := range []string{
			"https://github.com/owner/repo/pull/123",
			"https://github.com/OWNER/REPO/pull/123",
			"https://github.com/<owner>/<repo>/pull/123",
			"https://github.com/org/repo/pull/123",
			"https://github.com/user/repo/pull/123",
		} {
			logContent := `{"type":"tool_result","content":"` + placeholder + `\n"}
{"type":"result","total_cost_usd":0.5,"duration_ms":5000}
`
			state, store := setupFinalizeTest(t, logContent)
			if _, err := finalizeFromLog(store, state); err != nil {
				t.Fatalf("finalizeFromLog() error: %v", err)
			}
			if state.PRURL != nil {
				t.Errorf("expected nil PRURL for placeholder %q, got %q", placeholder, *state.PRURL)
			}
		}
	})
}

func TestExtractClaudeSessionID(t *testing.T) {
	t.Run("extracts session_id from result event", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5-20250929"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it..."}]}}
{"type":"result","session_id":"a1b2c3d4-e5f6-7890-abcd-ef1234567890","total_cost_usd":1.5,"duration_ms":30000}
`
		logFile := writeTestLog(t, logContent)
		got := ExtractClaudeSessionID(logFile)
		if got != "a1b2c3d4-e5f6-7890-abcd-ef1234567890" {
			t.Errorf("ExtractClaudeSessionID() = %q, want UUID", got)
		}
	})

	t.Run("returns empty when no result event", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}
`
		logFile := writeTestLog(t, logContent)
		got := ExtractClaudeSessionID(logFile)
		if got != "" {
			t.Errorf("ExtractClaudeSessionID() = %q, want empty", got)
		}
	})

	t.Run("returns empty when result has no session_id", func(t *testing.T) {
		logContent := `{"type":"result","total_cost_usd":0.5,"duration_ms":1000}
`
		logFile := writeTestLog(t, logContent)
		got := ExtractClaudeSessionID(logFile)
		if got != "" {
			t.Errorf("ExtractClaudeSessionID() = %q, want empty", got)
		}
	})

	t.Run("returns empty for nonexistent file", func(t *testing.T) {
		got := ExtractClaudeSessionID("/nonexistent/path/log.jsonl")
		if got != "" {
			t.Errorf("ExtractClaudeSessionID() = %q, want empty", got)
		}
	})

	t.Run("survives malformed lines", func(t *testing.T) {
		logContent := `not json
{"truncated":
{"type":"result","session_id":"deadbeef-1234-5678-9abc-def012345678","total_cost_usd":0.1}
`
		logFile := writeTestLog(t, logContent)
		got := ExtractClaudeSessionID(logFile)
		if got != "deadbeef-1234-5678-9abc-def012345678" {
			t.Errorf("ExtractClaudeSessionID() = %q, want UUID", got)
		}
	})
}

func TestEmitFinalizeEvents(t *testing.T) {
	cost := 1.5
	dur := int64(30000)
	prURL := "https://github.com/owner/repo/pull/42"

	t.Run("failed run emits needs-attention and nothing else", func(t *testing.T) {
		baseDir := t.TempDir()
		reason := "error_during_execution: No conversation found"
		state := &run.State{
			ID:            "20260628-0900-fail",
			CostUSD:       &cost,
			DurationMS:    &dur,
			PRURL:         &prURL, // even with a stale URL, no pr-created on crash
			FailureReason: &reason,
		}

		emitFinalizeEvents(baseDir, state, nil)

		types := eventTypesFor(t, baseDir, state.ID)
		if len(types) != 1 || types[0] != event.AgentNeedsAttention {
			t.Fatalf("expected only %q, got %v", event.AgentNeedsAttention, types)
		}
	})

	t.Run("successful run with PR emits completed and pr-created", func(t *testing.T) {
		baseDir := t.TempDir()
		state := &run.State{
			ID:         "20260628-0901-ok",
			CostUSD:    &cost,
			DurationMS: &dur,
			PRURL:      &prURL,
		}

		emitFinalizeEvents(baseDir, state, nil)

		types := eventTypesFor(t, baseDir, state.ID)
		if !containsEvent(types, event.AgentCompleted) {
			t.Errorf("expected %q in %v", event.AgentCompleted, types)
		}
		if !containsEvent(types, event.AgentPRCreated) {
			t.Errorf("expected %q in %v", event.AgentPRCreated, types)
		}
		if containsEvent(types, event.AgentNeedsAttention) {
			t.Errorf("did not expect %q in %v", event.AgentNeedsAttention, types)
		}
	})

	t.Run("successful run without PR emits completed only", func(t *testing.T) {
		baseDir := t.TempDir()
		state := &run.State{
			ID:         "20260628-0902-nopr",
			CostUSD:    &cost,
			DurationMS: &dur,
		}

		emitFinalizeEvents(baseDir, state, nil)

		types := eventTypesFor(t, baseDir, state.ID)
		if len(types) != 1 || types[0] != event.AgentCompleted {
			t.Fatalf("expected only %q, got %v", event.AgentCompleted, types)
		}
	})
}

func eventTypesFor(t *testing.T, baseDir, runID string) []string {
	t.Helper()
	evts, err := event.NewLog(baseDir).Read()
	if err != nil {
		t.Fatalf("reading event log: %v", err)
	}
	var types []string
	for _, e := range evts {
		if e.RunID == runID {
			types = append(types, e.Type)
		}
	}
	return types
}

func containsEvent(types []string, want string) bool {
	for _, ty := range types {
		if ty == want {
			return true
		}
	}
	return false
}

func writeTestLog(t *testing.T, content string) string {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "test.jsonl")
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("writing log file: %v", err)
	}
	return logFile
}

// setupFinalizeTest creates a temporary log file and state for testing finalizeFromLog.
func setupFinalizeTest(t *testing.T, logContent string) (*run.State, run.StateStore) {
	t.Helper()
	dir := t.TempDir()

	logFile := filepath.Join(dir, "test.jsonl")
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("writing log file: %v", err)
	}

	stateDir := filepath.Join(dir, "runs")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("creating state dir: %v", err)
	}

	// Fixture origin so PR URL checks don't depend on this checkout.
	cloneDir := repoWithOrigin(t, "https://github.com/patflynn/klaus.git")
	state := &run.State{
		ID:       "test-run",
		LogFile:  &logFile,
		CloneDir: &cloneDir,
	}

	store := &testStateStore{dir: stateDir, state: state}
	return state, store
}

// testStateStore is a minimal StateStore for testing.
type testStateStore struct {
	dir   string
	state *run.State
}

func (s *testStateStore) Save(state *run.State) error {
	s.state = state
	return nil
}

func (s *testStateStore) Load(id string) (*run.State, error) {
	return s.state, nil
}

func (s *testStateStore) List() ([]*run.State, error) {
	return []*run.State{s.state}, nil
}

func (s *testStateStore) Delete(id string) error {
	return nil
}

func (s *testStateStore) StateDir() string {
	return s.dir
}

func (s *testStateStore) LogDir() string {
	return s.dir
}

func (s *testStateStore) EnsureDirs() error {
	return nil
}

// TestEmitFinalizeEventsNilState covers the gemini #277 follow-up: the finalize
// path must no-op rather than dereference a nil run state.
func TestEmitFinalizeEventsNilState(t *testing.T) {
	// Should return early without panicking.
	emitFinalizeEvents(t.TempDir(), nil, nil)
}

func assertPRURL(t *testing.T, state *run.State, want string) {
	t.Helper()
	if state.PRURL == nil {
		t.Fatalf("expected PRURL %q, got nil", want)
	}
	if *state.PRURL != want {
		t.Errorf("PRURL = %q, want %q", *state.PRURL, want)
	}
}

func assertCost(t *testing.T, state *run.State, want float64) {
	t.Helper()
	if state.CostUSD == nil {
		t.Fatalf("expected CostUSD %v, got nil", want)
	}
	if *state.CostUSD != want {
		t.Errorf("CostUSD = %v, want %v", *state.CostUSD, want)
	}
}

func assertDuration(t *testing.T, state *run.State, want int64) {
	t.Helper()
	if state.DurationMS == nil {
		t.Fatalf("expected DurationMS %v, got nil", want)
	}
	if *state.DurationMS != want {
		t.Errorf("DurationMS = %v, want %v", *state.DurationMS, want)
	}
}

// finalizeRealRepo runs _finalize for a run on worktree/branch with the given
// log. gh is stubbed (empty output); git is real.
func finalizeRealRepo(t *testing.T, repo, worktree, branch, logContent string, prURL *string) (*run.HomeDirStore, *run.State, *fakeRunner) {
	t.Helper()
	sessionID := "20260918-1205-salvage-session"
	t.Setenv("HOME", t.TempDir())
	t.Setenv(sessionIDEnv, sessionID)
	store, err := run.NewHomeDirStore(sessionID)
	if err != nil {
		t.Fatalf("NewHomeDirStore: %v", err)
	}
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	runID := "20260918-1205-salvage"
	logFile := filepath.Join(store.LogDir(), runID+".jsonl")
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("writing log: %v", err)
	}
	state := &run.State{
		ID:        runID,
		Prompt:    "rename the thing",
		Branch:    branch,
		Worktree:  worktree,
		CreatedAt: "2026-09-18T12:05:00Z",
		LogFile:   &logFile,
		CloneDir:  &repo,
		PRURL:     prURL,
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("saving state: %v", err)
	}

	r := &fakeRunner{}
	prev := budgetPauseRunner
	budgetPauseRunner = r
	t.Cleanup(func() { budgetPauseRunner = prev })

	finalizeCmd.SetContext(context.Background())
	if err := finalizeCmd.RunE(finalizeCmd, []string{runID}); err != nil {
		t.Fatalf("_finalize: %v", err)
	}
	final, err := store.Load(runID)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	return store, final, r
}

func runEvents(t *testing.T, baseDir, runID string) map[string]map[string]interface{} {
	t.Helper()
	evts, err := event.NewLog(baseDir).Read()
	if err != nil {
		t.Fatalf("reading events: %v", err)
	}
	byType := map[string]map[string]interface{}{}
	for _, e := range evts {
		if e.RunID == runID {
			byType[e.Type] = e.Data
		}
	}
	return byType
}

func gitOut(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Regression for #299: a session-limit stop (subtype success, no PR) must not
// destroy the agent's uncommitted work or its branch.
func TestFinalizeSalvagesRunWithoutPR(t *testing.T) {
	sessionLimitLog := `{"type":"assistant","message":{"content":[{"type":"text","text":"VM test passes."}]}}
{"type":"result","subtype":"success","is_error":true,"result":"You've hit your session limit · resets 4:20pm","total_cost_usd":3.1,"duration_ms":2100000}
{"type":"klaus_exit","exit_code":1}
`
	t.Run("commits, pushes, and keeps the branch", func(t *testing.T) {
		origin, repo, worktree, branch := setupBareRemote(t)

		store, state, r := finalizeRealRepo(t, repo, worktree, branch, sessionLimitLog, nil)

		if _, err := gitOut(t, repo, "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
			t.Errorf("local branch deleted: %v", err)
		}
		files, err := gitOut(t, origin, "show", "--name-only", "--format=%s", branch)
		if err != nil {
			t.Fatalf("branch missing on origin: %v (%s)", err, files)
		}
		if !strings.Contains(files, "wip: klaus finalize salvage "+state.ID) || !strings.Contains(files, "wip.txt") {
			t.Errorf("origin tip should be the WIP commit with wip.txt, got:\n%s", files)
		}
		if _, err := os.Stat(worktree); !os.IsNotExist(err) {
			t.Errorf("worktree should be removed once the branch is safe, stat err = %v", err)
		}
		if state.NeedsAttention == nil || !strings.Contains(*state.NeedsAttention, branch) {
			t.Errorf("NeedsAttention should name the branch, got %v", state.NeedsAttention)
		}
		if len(r.ghCalls) != 0 {
			t.Errorf("salvage must not touch GitHub, got gh calls %v", r.ghCalls)
		}

		evts := runEvents(t, store.BaseDir(), state.ID)
		if _, ok := evts[event.AgentCompleted]; ok {
			t.Error("agent:completed must not be emitted for a salvaged run")
		}
		na, ok := evts[event.AgentNeedsAttention]
		if !ok {
			t.Fatalf("expected agent:needs-attention, got %v", evts)
		}
		if na["branch"] != branch || na["pushed"] != true || na["reason"] != "session_limit" {
			t.Errorf("needs-attention data = %v", na)
		}
	})

	t.Run("push failure keeps the local branch", func(t *testing.T) {
		origin, repo, worktree, branch := setupBareRemote(t)
		runGitCmd(t, repo, "remote", "set-url", "origin", origin+"-gone")

		store, state, _ := finalizeRealRepo(t, repo, worktree, branch, sessionLimitLog, nil)

		files, err := gitOut(t, repo, "show", "--name-only", "--format=%s", "refs/heads/"+branch)
		if err != nil {
			t.Fatalf("local branch deleted after failed push: %v", err)
		}
		if !strings.Contains(files, "wip.txt") {
			t.Errorf("local branch should carry the WIP commit, got:\n%s", files)
		}
		na := runEvents(t, store.BaseDir(), state.ID)[event.AgentNeedsAttention]
		if na == nil || na["pushed"] != false || na["branch"] != branch {
			t.Errorf("needs-attention data = %v", na)
		}
	})

	cleanLog := `{"type":"result","subtype":"success","result":"Pushed the branch.","total_cost_usd":1,"duration_ms":1000}
`
	t.Run("already-pushed branch (direct-push) is reported, not re-pushed", func(t *testing.T) {
		_, repo, worktree, branch := setupBareRemote(t)
		commitAndPush(t, worktree, branch)

		store, state, _ := finalizeRealRepo(t, repo, worktree, branch, cleanLog, nil)

		na := runEvents(t, store.BaseDir(), state.ID)[event.AgentNeedsAttention]
		if na == nil || na["pushed"] != true || na["reason"] != "no_pr" || na["branch"] != branch {
			t.Errorf("needs-attention data = %v", na)
		}
		if subj, _ := gitOut(t, repo, "log", "-1", "--format=%s", "origin/"+branch); subj != "feat: done" {
			t.Errorf("clean tree must not get a WIP commit; origin tip = %q", subj)
		}
	})

	// A stale origin/<branch> must not count as pushed.
	t.Run("remote branch deleted since last fetch is pushed again", func(t *testing.T) {
		origin, repo, worktree, branch := setupBareRemote(t)
		commitAndPush(t, worktree, branch)
		runGitCmd(t, origin, "branch", "-D", branch) // no fetch: origin/<branch> is stale

		store, state, _ := finalizeRealRepo(t, repo, worktree, branch, cleanLog, nil)

		local, _ := gitOut(t, repo, "rev-parse", "refs/heads/"+branch)
		if remote, err := gitOut(t, origin, "rev-parse", "--verify", "refs/heads/"+branch); err != nil || remote != local {
			t.Fatalf("branch not re-pushed: origin=%q (%v), local=%q", remote, err, local)
		}
		na := runEvents(t, store.BaseDir(), state.ID)[event.AgentNeedsAttention]
		if na == nil || na["pushed"] != true {
			t.Errorf("needs-attention data = %v", na)
		}
	})

	t.Run("unreachable origin with stale ref reports pushed false", func(t *testing.T) {
		origin, repo, worktree, branch := setupBareRemote(t)
		commitAndPush(t, worktree, branch)
		runGitCmd(t, repo, "remote", "set-url", "origin", origin+"-gone")

		store, state, _ := finalizeRealRepo(t, repo, worktree, branch, cleanLog, nil)

		if _, err := gitOut(t, repo, "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
			t.Errorf("local branch deleted: %v", err)
		}
		na := runEvents(t, store.BaseDir(), state.ID)[event.AgentNeedsAttention]
		if na == nil || na["pushed"] != false {
			t.Errorf("unverified remote must report pushed=false, got %v", na)
		}
	})
}

// commitAndPush commits the worktree's changes and pushes the branch, leaving
// origin/<branch> equal to the local tip.
func commitAndPush(t *testing.T, worktree, branch string) {
	t.Helper()
	runGitCmd(t, worktree, "add", "-A")
	runGitCmd(t, worktree, "commit", "-m", "feat: done")
	runGitCmd(t, worktree, "push", "-u", "origin", branch)
}

func TestFinalizeWithPRDeletesBranch(t *testing.T) {
	_, repo, worktree, branch := setupBareRemote(t)
	commitAndPush(t, worktree, branch)
	prLog := `{"type":"assistant","message":{"content":[{"type":"text","text":"Opened https://github.com/acme/widget/pull/9"}]}}
{"type":"result","subtype":"success","total_cost_usd":1,"duration_ms":1000}
`
	store, state, _ := finalizeRealRepo(t, repo, worktree, branch, prLog, nil)

	if _, err := gitOut(t, repo, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Error("local branch should be deleted after a PR run")
	}
	if state.NeedsAttention != nil {
		t.Errorf("NeedsAttention = %q, want nil", *state.NeedsAttention)
	}
	evts := runEvents(t, store.BaseDir(), state.ID)
	if _, ok := evts[event.AgentNeedsAttention]; ok {
		t.Error("PR run must not emit agent:needs-attention")
	}
	if _, ok := evts[event.AgentCompleted]; !ok {
		t.Error("expected agent:completed")
	}
	if _, ok := evts[event.AgentPRCreated]; !ok {
		t.Error("expected agent:pr-created")
	}
}

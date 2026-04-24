package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
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
		cleanupWorktree(context.Background(), store, git.NewExecClient(), state)

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
		cleanupWorktree(context.Background(), store, git.NewExecClient(), state)

		if state.Worktree != "" {
			t.Errorf("expected Worktree to be cleared, got %q", state.Worktree)
		}
	})

	t.Run("no-op when worktree field is empty", func(t *testing.T) {
		state := &run.State{ID: "test-run", Worktree: ""}
		store := &testStateStore{state: state}

		cleanupWorktree(context.Background(), store, git.NewExecClient(), state)

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

// fakeTmux is a minimal tmux.Client for testing killAgentPane.
type fakeTmux struct {
	tmux.ExecClient
	killedPanes []string
}

func (f *fakeTmux) KillPane(_ context.Context, id string) error {
	f.killedPanes = append(f.killedPanes, id)
	return nil
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

func TestFinalizeFromLog(t *testing.T) {
	t.Run("extracts PR URL from assistant text", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5-20250929"}
{"type":"assistant","message":{"content":[{"type":"text","text":"I'll create the PR now."}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Created PR at https://github.com/owner/repo/pull/42"}]}}
{"type":"result","total_cost_usd":1.5,"duration_ms":30000}
`
		state, store := setupFinalizeTest(t, logContent)
		if err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/owner/repo/pull/42")
		assertCost(t, state, 1.5)
		assertDuration(t, state, 30000)
	})

	t.Run("extracts PR URL from tool_result event", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5-20250929"}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"gh pr create --title test"}}]}}
{"type":"tool_result","content":"https://github.com/owner/repo/pull/99\n"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Done! I created the PR."}]}}
{"type":"result","total_cost_usd":2.0,"duration_ms":45000}
`
		state, store := setupFinalizeTest(t, logContent)
		if err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/owner/repo/pull/99")
	})

	t.Run("extracts PR URL from user message with tool_result content", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5-20250929"}
{"type":"user","message":{"content":[{"type":"tool_result","content":"https://github.com/owner/repo/pull/7\n"}]}}
{"type":"result","total_cost_usd":1.0,"duration_ms":10000}
`
		state, store := setupFinalizeTest(t, logContent)
		if err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/owner/repo/pull/7")
	})

	t.Run("handles markdown link in assistant text", func(t *testing.T) {
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"Created [PR #42](https://github.com/owner/repo/pull/42) for review."}]}}
{"type":"result","total_cost_usd":1.0,"duration_ms":5000}
`
		state, store := setupFinalizeTest(t, logContent)
		if err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/owner/repo/pull/42")
	})

	t.Run("survives malformed JSONL lines", func(t *testing.T) {
		logContent := `{"type":"system","subtype":"init"}
not valid json at all
{"truncated":
{"type":"assistant","message":{"content":[{"type":"text","text":"PR: https://github.com/owner/repo/pull/5"}]}}
{"type":"result","total_cost_usd":0.5,"duration_ms":2000}
`
		state, store := setupFinalizeTest(t, logContent)
		if err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/owner/repo/pull/5")
		assertCost(t, state, 0.5)
	})

	t.Run("no PR URL in log", func(t *testing.T) {
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"Just doing some work."}]}}
{"type":"result","total_cost_usd":0.1,"duration_ms":1000}
`
		state, store := setupFinalizeTest(t, logContent)
		if err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		if state.PRURL != nil {
			t.Errorf("expected nil PRURL, got %q", *state.PRURL)
		}
	})

	t.Run("last PR URL wins", func(t *testing.T) {
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"First PR: https://github.com/owner/repo/pull/1"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Recreated PR: https://github.com/owner/repo/pull/2"}]}}
{"type":"result","total_cost_usd":1.0,"duration_ms":5000}
`
		state, store := setupFinalizeTest(t, logContent)
		if err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/owner/repo/pull/2")
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
		existing := "https://github.com/owner/repo/pull/42"
		state.PRURL = &existing

		if err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/owner/repo/pull/42")
	})

	t.Run("extracts PRURL from log when not set before finalization", func(t *testing.T) {
		// Simulates new-PR mode: state.PRURL is nil until the agent runs
		// `gh pr create`. Regex extraction fills it in from the log.
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"Creating PR now."}]}}
{"type":"tool_result","content":"https://github.com/owner/repo/pull/77\n"}
{"type":"result","total_cost_usd":1.0,"duration_ms":5000}
`
		state, store := setupFinalizeTest(t, logContent)
		if state.PRURL != nil {
			t.Fatalf("test precondition: expected nil PRURL before finalize")
		}

		if err := finalizeFromLog(store, state); err != nil {
			t.Fatalf("finalizeFromLog() error: %v", err)
		}
		assertPRURL(t, state, "https://github.com/owner/repo/pull/77")
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

	state := &run.State{
		ID:      "test-run",
		LogFile: &logFile,
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

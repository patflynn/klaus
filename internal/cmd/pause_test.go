package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/draft"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
)

// fakeRunner records gh/git calls and lets a test stub gh responses by
// matching substrings in the joined args. git calls are forwarded to the
// real git binary so worktree manipulation actually happens — that's the
// whole point of these tests (validate the WIP commit + push reach the
// filesystem).
type fakeRunner struct {
	realGit draft.ExecRunner
	ghCalls [][]string
	ghStubs []ghStub
}

type ghStub struct {
	match []string
	out   string
	err   error
}

func (f *fakeRunner) Git(ctx context.Context, workdir string, args ...string) (string, error) {
	return f.realGit.Git(ctx, workdir, args...)
}

func (f *fakeRunner) GH(_ context.Context, _ string, args ...string) (string, error) {
	cp := append([]string(nil), args...)
	f.ghCalls = append(f.ghCalls, cp)
	joined := strings.Join(args, " ")
	for _, s := range f.ghStubs {
		all := true
		for _, m := range s.match {
			if !strings.Contains(joined, m) {
				all = false
				break
			}
		}
		if all {
			return s.out, s.err
		}
	}
	return "", nil
}

func (f *fakeRunner) findCall(parts ...string) []string {
	for _, c := range f.ghCalls {
		all := true
		for _, p := range parts {
			found := false
			for _, a := range c {
				if a == p {
					found = true
					break
				}
			}
			if !found {
				all = false
				break
			}
		}
		if all {
			return c
		}
	}
	return nil
}

// setupBareRemote creates a bare repo to act as origin, an initial clone, a
// worktree with one dirty file, and returns the paths.
func setupBareRemote(t *testing.T) (origin, repo, worktree, branch string) {
	t.Helper()
	root := t.TempDir()

	origin = filepath.Join(root, "origin.git")
	runGitCmd(t, root, "init", "--bare", origin)

	repo = filepath.Join(root, "repo")
	runGitCmd(t, root, "clone", origin, repo)
	runGitCmd(t, repo, "config", "user.email", "test@test")
	runGitCmd(t, repo, "config", "user.name", "test")

	// Seed initial commit on main.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, repo, "add", "README.md")
	runGitCmd(t, repo, "commit", "-m", "init")
	runGitCmd(t, repo, "branch", "-M", "main")
	runGitCmd(t, repo, "push", "-u", "origin", "main")

	branch = "agent/test-pause"
	worktree = filepath.Join(root, "wt")
	runGitCmd(t, repo, "worktree", "add", worktree, "-b", branch)
	runGitCmd(t, worktree, "config", "user.email", "test@test")
	runGitCmd(t, worktree, "config", "user.name", "test")

	// Add a dirty file (uncommitted) to simulate work-in-progress.
	if err := os.WriteFile(filepath.Join(worktree, "wip.txt"), []byte("partial work\n"), 0644); err != nil {
		t.Fatal(err)
	}

	return origin, repo, worktree, branch
}

func TestBudgetPauseFlow_EndToEnd(t *testing.T) {
	_, _, worktree, branch := setupBareRemote(t)

	r := &fakeRunner{}
	// gh pr list returns nothing → triggers create.
	r.ghStubs = append(r.ghStubs,
		ghStub{match: []string{"pr", "list", "--head"}, out: "", err: nil},
		ghStub{match: []string{"pr", "create", "--draft"}, out: "https://github.com/owner/repo/pull/777\n", err: nil},
	)

	in := draft.PauseInput{
		RunID:     "20260529-1729-zzzz",
		Worktree:  worktree,
		Branch:    branch,
		Repo:      "owner/repo",
		Prompt:    "fix the auth bug",
		CostUSD:   4.98,
		BudgetUSD: 5.00,
	}
	out, err := draft.HandleBudgetPause(context.Background(), r, in)
	if err != nil {
		t.Fatalf("HandleBudgetPause: %v", err)
	}

	if !out.CommittedWIP {
		t.Error("expected CommittedWIP=true (worktree had dirty file)")
	}
	if !out.CreatedNewPR {
		t.Error("expected CreatedNewPR=true (no existing PR)")
	}
	if out.PRNumber != "777" {
		t.Errorf("expected PRNumber=777, got %q", out.PRNumber)
	}

	// Verify the WIP commit landed: git log on the branch must show a
	// commit referencing the run ID.
	logOut, err := r.realGit.Git(context.Background(), worktree, "log", "-1", "--format=%s")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(logOut, "WIP from klaus run 20260529-1729-zzzz") {
		t.Errorf("expected WIP commit message, got %q", strings.TrimSpace(logOut))
	}

	// Verify the branch was pushed to origin.
	listOut, err := r.realGit.Git(context.Background(), worktree, "ls-remote", "origin", branch)
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	if strings.TrimSpace(listOut) == "" {
		t.Error("expected branch pushed to origin, got empty ls-remote")
	}

	// Verify gh was driven through the pause sequence.
	if r.findCall("label", "create", "klaus:budget-paused") == nil {
		t.Error("expected gh label create call")
	}
	if r.findCall("pr", "create", "--draft", "--head", branch) == nil {
		t.Error("expected gh pr create --draft call")
	}
	if r.findCall("pr", "edit", "777", "--add-label") == nil {
		t.Error("expected gh pr edit --add-label call")
	}
	if c := r.findCall("pr", "comment", "777"); c == nil {
		t.Error("expected gh pr comment call")
	} else {
		joined := strings.Join(c, " ")
		if !strings.Contains(joined, "$4.98") || !strings.Contains(joined, "$5.00") {
			t.Errorf("expected comment body with cost/budget, got %q", joined)
		}
	}
}

func TestBudgetPauseFlow_ReusesExistingPR(t *testing.T) {
	_, _, worktree, branch := setupBareRemote(t)

	r := &fakeRunner{}
	r.ghStubs = append(r.ghStubs,
		// lookup existing PR returns number+url
		ghStub{match: []string{"pr", "view", "55", "--repo", "owner/repo", "--json"}, out: "55\nhttps://github.com/owner/repo/pull/55\n"},
	)

	in := draft.PauseInput{
		RunID:      "20260529-1729-aaaa",
		Worktree:   worktree,
		Branch:     branch,
		Repo:       "owner/repo",
		Prompt:     "second attempt",
		CostUSD:    4.95,
		BudgetUSD:  5.00,
		ExistingPR: "55",
	}
	out, err := draft.HandleBudgetPause(context.Background(), r, in)
	if err != nil {
		t.Fatalf("HandleBudgetPause: %v", err)
	}

	if out.CreatedNewPR {
		t.Error("expected to reuse PR #55, not create a new one")
	}
	if out.PRNumber != "55" {
		t.Errorf("expected PR #55, got %q", out.PRNumber)
	}
	// Should not have called pr create.
	if c := r.findCall("pr", "create"); c != nil {
		t.Errorf("expected no gh pr create, got %v", c)
	}
	// Should still apply label and comment.
	if r.findCall("pr", "edit", "55", "--add-label") == nil {
		t.Error("expected label apply on existing PR")
	}
	if r.findCall("pr", "comment", "55") == nil {
		t.Error("expected PR comment on existing PR")
	}
}

func TestIsBudgetExhausted(t *testing.T) {
	cost := 4.95
	budget5 := "5.00"
	budget10 := "10.00"
	tests := []struct {
		name    string
		state   *run.State
		subtype string
		want    bool
	}{
		{
			name:    "95 percent with non-success subtype",
			state:   &run.State{CostUSD: &cost, Budget: &budget5},
			subtype: "",
			want:    true,
		},
		{
			name:    "95 percent but success subtype",
			state:   &run.State{CostUSD: &cost, Budget: &budget5},
			subtype: "success",
			want:    false,
		},
		{
			name:    "below 95 percent",
			state:   &run.State{CostUSD: &cost, Budget: &budget10},
			subtype: "",
			want:    false,
		},
		{
			name:    "no budget",
			state:   &run.State{CostUSD: &cost},
			subtype: "",
			want:    false,
		},
		{
			name:    "no cost",
			state:   &run.State{Budget: &budget5},
			subtype: "",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBudgetExhausted(tt.state, tt.subtype)
			if got != tt.want {
				t.Errorf("isBudgetExhausted = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFinalizeBudgetPauseEmitsEvents drives the full _finalize budget-pause
// path end-to-end: a dirty worktree + a result-event-less log + cost near
// the cap → expects the WIP commit, the gh pause calls, agent:paused +
// agent:pr-created events in events.jsonl, and the worktree removed.
func TestFinalizeBudgetPauseEmitsEvents(t *testing.T) {
	_, repo, worktree, branch := setupBareRemote(t)

	sessionID := "20260529-1729-test-session"
	// Set up the HomeDirStore at a temp KLAUS sessions dir.
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(sessionIDEnv, sessionID)
	store, err := run.NewHomeDirStore(sessionID)
	if err != nil {
		t.Fatalf("NewHomeDirStore: %v", err)
	}
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	// Build a fake log file with cost near budget cap, no success subtype.
	runID := "20260529-1729-runxx"
	logFile := filepath.Join(store.LogDir(), runID+".jsonl")
	logContent := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working..."}]}}
{"type":"result","subtype":"error_max_turns","total_cost_usd":4.95,"duration_ms":30000}
`
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("writing log: %v", err)
	}

	budget := "5.00"
	targetRepo := "owner/repo"
	state := &run.State{
		ID:         runID,
		Prompt:     "fix the auth bug in handler",
		Branch:     branch,
		Worktree:   worktree,
		CreatedAt:  "2026-05-29T17:00:00Z",
		LogFile:    &logFile,
		Budget:     &budget,
		TargetRepo: &targetRepo,
		CloneDir:   &repo,
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("saving state: %v", err)
	}

	// Inject the fake gh runner; keep real git.
	r := &fakeRunner{}
	r.ghStubs = []ghStub{
		{match: []string{"pr", "list", "--head"}, out: ""},
		{match: []string{"pr", "create", "--draft"}, out: "https://github.com/owner/repo/pull/123\n"},
	}
	prev := budgetPauseRunner
	budgetPauseRunner = r
	defer func() { budgetPauseRunner = prev }()

	// Run _finalize.
	finalizeCmd.SetContext(context.Background())
	if err := finalizeCmd.RunE(finalizeCmd, []string{runID}); err != nil {
		t.Fatalf("_finalize: %v", err)
	}

	// Assert: gh pause calls were issued.
	if r.findCall("pr", "create", "--draft") == nil {
		t.Error("expected gh pr create --draft (no prior PR was set on state)")
	}
	if r.findCall("pr", "edit", "123", "--add-label") == nil {
		t.Error("expected gh label apply on PR 123")
	}
	if r.findCall("pr", "comment", "123") == nil {
		t.Error("expected gh pr comment on PR 123")
	}

	// Assert: events log contains agent:paused.
	evtFile := filepath.Join(store.BaseDir(), "events.jsonl")
	data, err := os.ReadFile(evtFile)
	if err != nil {
		t.Fatalf("reading events.jsonl: %v", err)
	}
	pausedFound := false
	prCreatedFound := false
	completedFound := false
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var ev struct {
			RunID string                 `json:"run_id"`
			Type  string                 `json:"type"`
			Data  map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type == event.AgentPaused && ev.RunID == runID {
			pausedFound = true
			if got, _ := ev.Data["pr_number"].(string); got != "123" {
				t.Errorf("agent:paused pr_number = %q, want 123", got)
			}
		}
		if ev.Type == event.AgentPRCreated && ev.RunID == runID {
			prCreatedFound = true
		}
		if ev.Type == event.AgentCompleted && ev.RunID == runID {
			completedFound = true
		}
	}
	if !pausedFound {
		t.Error("expected agent:paused event in events.jsonl")
	}
	if !prCreatedFound {
		t.Error("expected agent:pr-created event in events.jsonl (PR was discovered)")
	}
	if completedFound {
		t.Error("agent:completed should be suppressed when the run is paused")
	}

	// Assert: worktree was cleaned up by _finalize's cleanupWorktree step.
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("expected worktree removed after _finalize, stat err = %v", err)
	}

	// Assert: the WIP commit was pushed to origin.
	pushed, err := r.realGit.Git(context.Background(), repo, "ls-remote", "origin", branch)
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	if strings.TrimSpace(pushed) == "" {
		t.Error("expected branch pushed to origin, got empty ls-remote")
	}
}

// TestFinalizeClearsBudgetPausedLabel verifies that a normal-completion
// _finalize against a PR that carries the budget-paused label removes the
// label and emits agent:resumed.
func TestFinalizeClearsBudgetPausedLabel(t *testing.T) {
	_, repo, worktree, branch := setupBareRemote(t)

	sessionID := "20260529-1729-clear-session"
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(sessionIDEnv, sessionID)
	store, err := run.NewHomeDirStore(sessionID)
	if err != nil {
		t.Fatalf("NewHomeDirStore: %v", err)
	}
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	// Log: success subtype, cost well below cap.
	runID := "20260529-1729-runzz"
	logFile := filepath.Join(store.LogDir(), runID+".jsonl")
	logContent := `{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":10000}
`
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("writing log: %v", err)
	}

	prNum := "200"
	prURL := "https://github.com/owner/repo/pull/200"
	budget := "5.00"
	targetRepo := "owner/repo"
	state := &run.State{
		ID:         runID,
		Prompt:     "finish what the paused agent started",
		PR:         &prNum,
		PRURL:      &prURL,
		Branch:     branch,
		Worktree:   worktree,
		CreatedAt:  "2026-05-29T17:00:00Z",
		LogFile:    &logFile,
		Budget:     &budget,
		TargetRepo: &targetRepo,
		CloneDir:   &repo,
		Type:       "pr-fix",
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("saving state: %v", err)
	}

	// gh stubs: HasBudgetPausedLabel returns the label, ClearBudgetPausedLabel succeeds.
	r := &fakeRunner{}
	r.ghStubs = []ghStub{
		{match: []string{"pr", "view", "200", "--repo", "owner/repo", "--json", "labels"}, out: "klaus:budget-paused\n"},
		{match: []string{"pr", "edit", "200", "--repo", "owner/repo", "--remove-label"}, out: ""},
	}
	prev := budgetPauseRunner
	budgetPauseRunner = r
	defer func() { budgetPauseRunner = prev }()

	finalizeCmd.SetContext(context.Background())
	if err := finalizeCmd.RunE(finalizeCmd, []string{runID}); err != nil {
		t.Fatalf("_finalize: %v", err)
	}

	if r.findCall("pr", "edit", "200", "--remove-label") == nil {
		t.Error("expected gh pr edit --remove-label on follow-up finalize")
	}

	// Assert agent:resumed emitted, agent:completed also emitted.
	evtFile := filepath.Join(store.BaseDir(), "events.jsonl")
	data, _ := os.ReadFile(evtFile)
	if !strings.Contains(string(data), event.AgentResumed) {
		t.Error("expected agent:resumed event")
	}
	if !strings.Contains(string(data), event.AgentCompleted) {
		t.Error("expected agent:completed event on normal finalize")
	}
	// Verify we did NOT trigger the budget-pause flow this time.
	if r.findCall("pr", "create", "--draft") != nil {
		t.Error("should not run pause flow on a successful completion")
	}
}


package draft

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/event"
)

// recorderRunner is a Runner that captures every git/gh call and lets each
// test stub specific responses by command-prefix match.
type recorderRunner struct {
	gitCalls  []recorded
	ghCalls   []recorded
	gitStubs  []stub
	ghStubs   []stub
}

type recorded struct {
	workdir string
	args    []string
}

type stub struct {
	// match: substrings that must all appear in the joined args (space-separated)
	match []string
	out   string
	err   error
}

func (r *recorderRunner) addGitStub(match []string, out string, err error) {
	r.gitStubs = append(r.gitStubs, stub{match: match, out: out, err: err})
}
func (r *recorderRunner) addGHStub(match []string, out string, err error) {
	r.ghStubs = append(r.ghStubs, stub{match: match, out: out, err: err})
}

func (r *recorderRunner) Git(_ context.Context, workdir string, args ...string) (string, error) {
	r.gitCalls = append(r.gitCalls, recorded{workdir, args})
	joined := strings.Join(args, " ")
	for _, s := range r.gitStubs {
		if allContain(joined, s.match) {
			return s.out, s.err
		}
	}
	return "", nil
}

func (r *recorderRunner) GH(_ context.Context, workdir string, args ...string) (string, error) {
	r.ghCalls = append(r.ghCalls, recorded{workdir, args})
	joined := strings.Join(args, " ")
	for _, s := range r.ghStubs {
		if allContain(joined, s.match) {
			return s.out, s.err
		}
	}
	return "", nil
}

func allContain(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// argsContainAll reports whether every part appears as a standalone arg.
// Matches by exact arg equality so "pr" doesn't fuzzy-match "progress".
func argsContainAll(args, parts []string) bool {
	for _, p := range parts {
		found := false
		for _, a := range args {
			if a == p {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (r *recorderRunner) findGitCall(parts ...string) *recorded {
	for i := range r.gitCalls {
		if argsContainAll(r.gitCalls[i].args, parts) {
			return &r.gitCalls[i]
		}
	}
	return nil
}

func (r *recorderRunner) findGHCall(parts ...string) *recorded {
	for i := range r.ghCalls {
		if argsContainAll(r.ghCalls[i].args, parts) {
			return &r.ghCalls[i]
		}
	}
	return nil
}

func TestHandleBudgetPause_CreatesNewPRWithDirtyWorktree(t *testing.T) {
	r := &recorderRunner{}
	// Dirty worktree.
	r.addGitStub([]string{"status", "--porcelain"}, " M foo.go\n", nil)
	// findPRForBranch returns nothing (no existing PR).
	r.addGHStub([]string{"pr", "list", "--head"}, "", nil)
	// gh pr create returns the new PR URL.
	r.addGHStub([]string{"pr", "create"}, "https://github.com/owner/repo/pull/42\n", nil)

	in := PauseInput{
		RunID:     "20260101-1200-aaaa",
		Worktree:  "/tmp/wt",
		Branch:    "agent/20260101-1200-aaaa",
		Repo:      "owner/repo",
		Prompt:    "fix the auth bug",
		CostUSD:   4.95,
		BudgetUSD: 5.00,
	}
	out, err := HandleBudgetPause(context.Background(), r, in)
	if err != nil {
		t.Fatalf("HandleBudgetPause error: %v", err)
	}

	if !out.CommittedWIP {
		t.Error("expected CommittedWIP=true for dirty worktree")
	}
	if !out.CreatedNewPR {
		t.Error("expected CreatedNewPR=true when no PR existed")
	}
	if out.PRNumber != "42" {
		t.Errorf("expected PRNumber=42, got %q", out.PRNumber)
	}
	if out.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Errorf("expected PRURL set, got %q", out.PRURL)
	}

	// Verify the WIP commit happened.
	if r.findGitCall("add", "-A") == nil {
		t.Error("expected git add -A call")
	}
	if c := r.findGitCall("commit", "-m"); c == nil {
		t.Error("expected git commit call")
	} else {
		// Commit message should reference the run ID.
		if !strings.Contains(strings.Join(c.args, " "), "20260101-1200-aaaa") {
			t.Errorf("commit message should include run ID, got %v", c.args)
		}
	}
	// Verify the push.
	if c := r.findGitCall("push", "--force-with-lease"); c == nil {
		t.Error("expected git push --force-with-lease call")
	}
	// Verify label create.
	if c := r.findGHCall("label", "create", event.BudgetPausedLabel); c == nil {
		t.Error("expected gh label create call")
	}
	// Verify PR create with --draft.
	if c := r.findGHCall("pr", "create", "--draft"); c == nil {
		t.Error("expected gh pr create --draft call")
	}
	// Verify label apply.
	if c := r.findGHCall("pr", "edit", "42", "--add-label"); c == nil {
		t.Error("expected gh pr edit --add-label call")
	}
	// Verify PR comment.
	if c := r.findGHCall("pr", "comment", "42"); c == nil {
		t.Error("expected gh pr comment call")
	}
}

func TestHandleBudgetPause_UsesExistingPR(t *testing.T) {
	r := &recorderRunner{}
	// Clean worktree.
	r.addGitStub([]string{"status", "--porcelain"}, "", nil)
	// lookupPR returns the existing PR.
	r.addGHStub([]string{"pr", "view", "99", "--json"}, "99\nhttps://github.com/owner/repo/pull/99\n", nil)

	in := PauseInput{
		RunID:      "20260101-1200-bbbb",
		Worktree:   "/tmp/wt",
		Branch:     "agent/20260101-1200-bbbb",
		Repo:       "owner/repo",
		Prompt:     "do the thing",
		CostUSD:    4.99,
		BudgetUSD:  5.00,
		ExistingPR: "99",
	}
	out, err := HandleBudgetPause(context.Background(), r, in)
	if err != nil {
		t.Fatalf("HandleBudgetPause error: %v", err)
	}

	if out.CommittedWIP {
		t.Error("expected CommittedWIP=false for clean worktree")
	}
	if out.CreatedNewPR {
		t.Error("expected CreatedNewPR=false when PR existed")
	}
	if out.PRNumber != "99" {
		t.Errorf("expected PRNumber=99, got %q", out.PRNumber)
	}

	// Should NOT have called pr create.
	if r.findGHCall("pr", "create") != nil {
		t.Error("should not call gh pr create when PR already exists")
	}
	// Should still apply label and comment.
	if r.findGHCall("pr", "edit", "99", "--add-label") == nil {
		t.Error("expected label apply on existing PR")
	}
	if r.findGHCall("pr", "comment", "99") == nil {
		t.Error("expected PR comment on existing PR")
	}
}

func TestClearBudgetPausedLabel_NoOpWhenAbsent(t *testing.T) {
	r := &recorderRunner{}
	// gh succeeds with no output — label not present is not an error for gh
	// pr edit --remove-label, so we treat it as success.
	if err := ClearBudgetPausedLabel(context.Background(), r, "/tmp/wt", "owner/repo", "42"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c := r.findGHCall("pr", "edit", "42", "--remove-label"); c == nil {
		t.Error("expected gh pr edit --remove-label call")
	}
}

func TestClearBudgetPausedLabel_ToleratesNotFound(t *testing.T) {
	r := &recorderRunner{}
	r.addGHStub([]string{"--remove-label"}, "", fmt.Errorf("label not found"))
	if err := ClearBudgetPausedLabel(context.Background(), r, "/tmp/wt", "owner/repo", "42"); err != nil {
		t.Errorf("expected 'not found' to be tolerated, got %v", err)
	}
}

func TestHasBudgetPausedLabel(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    bool
		wantErr bool
	}{
		{"label present", "klaus:budget-paused\nother\n", true, false},
		{"label absent", "other\nfoo\n", false, false},
		{"no labels", "", false, false},
		{"only the label", "klaus:budget-paused", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recorderRunner{}
			r.addGHStub([]string{"pr", "view"}, tt.output, nil)
			got, err := HasBudgetPausedLabel(context.Background(), r, "/tmp/wt", "owner/repo", "42")
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBudgetExhausted(t *testing.T) {
	tests := []struct {
		cost float64
		cap  float64
		want bool
	}{
		{4.75, 5.00, true},   // at 95%
		{4.95, 5.00, true},   // at 99%
		{5.10, 5.00, true},   // overshoot
		{4.00, 5.00, false},  // at 80%
		{0.01, 5.00, false},  // tiny
		{4.95, 0, false},     // unknown cap
		{4.95, -1, false},    // bogus cap
	}
	for _, tt := range tests {
		got := BudgetExhausted(tt.cost, tt.cap)
		if got != tt.want {
			t.Errorf("BudgetExhausted(%v, %v) = %v, want %v", tt.cost, tt.cap, got, tt.want)
		}
	}
}

func TestExtractPRNumberFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/owner/repo/pull/42", "42"},
		{"https://github.com/owner/repo/pull/42\n", "42"},
		{"https://github.com/owner/repo/pull/123/files", "123"},
		{"not a url", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractPRNumberFromURL(tt.url)
		if got != tt.want {
			t.Errorf("extractPRNumberFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestTitleFromPrompt(t *testing.T) {
	tests := []struct {
		prompt string
		runID  string
		want   string
	}{
		{"fix the auth bug", "id", "[budget-paused] fix the auth bug"},
		{"line one\nline two", "id", "[budget-paused] line one"},
		{"", "myid", "klaus run myid (budget paused)"},
		{strings.Repeat("x", 100), "id", "[budget-paused] " + strings.Repeat("x", 72)},
	}
	for _, tt := range tests {
		got := titleFromPrompt(tt.prompt, tt.runID)
		if got != tt.want {
			t.Errorf("titleFromPrompt(%q, %q) = %q, want %q", tt.prompt, tt.runID, got, tt.want)
		}
	}
}

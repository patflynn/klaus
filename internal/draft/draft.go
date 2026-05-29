// Package draft handles the budget-pause persistence flow: when an agent
// exhausts its budget, klaus commits any work-in-progress to the agent's
// branch, pushes it, and ensures a draft PR exists carrying a
// "klaus:budget-paused" label plus an explanatory comment.
//
// The draft PR is the persistence layer for paused state — there is no
// in-process Status field. The pipeline FSM later reads the label off
// GitHub to surface the paused stage in the dashboard.
package draft

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/patflynn/klaus/internal/event"
)

// Runner abstracts the shell commands (git, gh) the pause flow needs.
// Tests inject a fake runner; production uses ExecRunner.
type Runner interface {
	// Git runs a git command in workdir and returns stdout. Stderr is
	// captured in the error.
	Git(ctx context.Context, workdir string, args ...string) (string, error)
	// GH runs a gh command (cwd irrelevant for most calls but accepted so
	// PR-create can run inside the worktree). Returns stdout, with stderr
	// captured in the error.
	GH(ctx context.Context, workdir string, args ...string) (string, error)
}

// ExecRunner runs real git/gh commands via os/exec.
type ExecRunner struct{}

func (ExecRunner) Git(ctx context.Context, workdir string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = workdir
	out, err := c.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return string(out), fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr))
	}
	return string(out), nil
}

func (ExecRunner) GH(ctx context.Context, workdir string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "gh", args...)
	c.Dir = workdir
	out, err := c.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return string(out), fmt.Errorf("gh %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr))
	}
	return string(out), nil
}

// PauseInput carries everything HandleBudgetPause needs to commit, push,
// and surface the paused state on GitHub.
type PauseInput struct {
	RunID      string  // klaus run ID
	Worktree   string  // path to the agent's worktree
	Branch     string  // branch name to push
	Repo       string  // owner/repo for gh commands; if empty, gh infers from cwd
	Prompt     string  // original agent prompt (used in WIP commit + PR body)
	CostUSD    float64 // observed spend
	BudgetUSD  float64 // budget cap (0 if unknown)
	ExistingPR string  // PR number if known; empty means "discover or create"
}

// PauseOutput reports what HandleBudgetPause observed/created.
type PauseOutput struct {
	PRNumber       string // the PR number (created or pre-existing)
	PRURL          string // the PR URL
	CreatedNewPR   bool   // true if HandleBudgetPause created the PR
	CommittedWIP   bool   // true if there were uncommitted changes that got committed
}

// HandleBudgetPause performs the WIP-commit + push + draft-PR + label + comment
// dance for budget exhaustion. Returns details of what was created so the
// caller can emit corresponding events.
//
// The flow is idempotent on the GitHub side: re-running against an already-
// labeled draft PR will not create duplicate PRs but will add a duplicate
// comment. Callers should not retry on success.
func HandleBudgetPause(ctx context.Context, r Runner, in PauseInput) (PauseOutput, error) {
	out := PauseOutput{}

	// Step 1: commit any uncommitted changes.
	committed, err := commitWIP(ctx, r, in)
	if err != nil {
		return out, fmt.Errorf("committing WIP: %w", err)
	}
	out.CommittedWIP = committed

	// Step 2: push the branch (force-with-lease tolerates a prior push).
	if err := pushBranch(ctx, r, in.Worktree, in.Branch); err != nil {
		return out, fmt.Errorf("pushing branch: %w", err)
	}

	// Step 3: ensure the budget-paused label exists in the repo.
	if err := ensureLabel(ctx, r, in.Worktree, in.Repo); err != nil {
		// Non-fatal: label creation failures shouldn't block the PR + comment.
		// gh label create --force will fail loudly if there's an auth problem,
		// but we still want to attempt PR creation.
		_ = err
	}

	// Step 4: find or create the PR.
	prNumber, prURL, created, err := ensurePR(ctx, r, in)
	if err != nil {
		return out, fmt.Errorf("ensuring PR: %w", err)
	}
	out.PRNumber = prNumber
	out.PRURL = prURL
	out.CreatedNewPR = created

	// Step 5: apply the label.
	if err := applyLabel(ctx, r, in.Worktree, in.Repo, prNumber); err != nil {
		return out, fmt.Errorf("applying label: %w", err)
	}

	// Step 6: post an explanatory PR comment.
	if err := postComment(ctx, r, in, prNumber); err != nil {
		return out, fmt.Errorf("posting comment: %w", err)
	}

	return out, nil
}

// ClearBudgetPausedLabel removes the klaus:budget-paused label from the
// given PR if it's present. Used by _finalize on follow-up runs against a
// paused PR so the dashboard reflects that the pause was resolved.
//
// Idempotent: if the label isn't present, this is a no-op.
func ClearBudgetPausedLabel(ctx context.Context, r Runner, workdir, repo, prNumber string) error {
	args := []string{"pr", "edit", prNumber, "--remove-label", event.BudgetPausedLabel}
	if repo != "" {
		args = []string{"pr", "edit", prNumber, "--repo", repo, "--remove-label", event.BudgetPausedLabel}
	}
	_, err := r.GH(ctx, workdir, args...)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return err
	}
	return nil
}

// HasBudgetPausedLabel reports whether the given PR currently carries the
// klaus:budget-paused label. Used at klaus launch --pr time to decide
// whether to emit agent:resumed.
func HasBudgetPausedLabel(ctx context.Context, r Runner, workdir, repo, prNumber string) (bool, error) {
	args := []string{"pr", "view", prNumber, "--json", "labels", "-q", ".labels[].name"}
	if repo != "" {
		args = []string{"pr", "view", prNumber, "--repo", repo, "--json", "labels", "-q", ".labels[].name"}
	}
	stdout, err := r.GH(ctx, workdir, args...)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == event.BudgetPausedLabel {
			return true, nil
		}
	}
	return false, nil
}

// ── internal helpers ────────────────────────────────────────────────────

func commitWIP(ctx context.Context, r Runner, in PauseInput) (bool, error) {
	// git status --porcelain returns empty if clean.
	status, err := r.Git(ctx, in.Worktree, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) == "" {
		return false, nil
	}
	if _, err := r.Git(ctx, in.Worktree, "add", "-A"); err != nil {
		return false, err
	}
	msg := fmt.Sprintf("WIP from klaus run %s (budget paused)\n\n%s", in.RunID, summarizePrompt(in.Prompt))
	if _, err := r.Git(ctx, in.Worktree, "commit", "-m", msg); err != nil {
		return false, err
	}
	return true, nil
}

func pushBranch(ctx context.Context, r Runner, workdir, branch string) error {
	// --force-with-lease is safe whether the branch was pushed before or not;
	// for first pushes git treats absent upstream as no constraint.
	// Use --set-upstream so subsequent pushes Just Work.
	_, err := r.Git(ctx, workdir, "push", "--force-with-lease", "--set-upstream", "origin", branch)
	return err
}

func ensureLabel(ctx context.Context, r Runner, workdir, repo string) error {
	args := []string{"label", "create", event.BudgetPausedLabel,
		"--force",
		"--description", "Agent paused at budget cap; work-in-progress committed to this branch.",
		"--color", "FBCA04",
	}
	if repo != "" {
		args = []string{"label", "create", event.BudgetPausedLabel,
			"--repo", repo,
			"--force",
			"--description", "Agent paused at budget cap; work-in-progress committed to this branch.",
			"--color", "FBCA04",
		}
	}
	_, err := r.GH(ctx, workdir, args...)
	return err
}

func ensurePR(ctx context.Context, r Runner, in PauseInput) (prNumber, prURL string, created bool, err error) {
	if in.ExistingPR != "" {
		num, url, lookupErr := lookupPR(ctx, r, in.Worktree, in.Repo, in.ExistingPR)
		if lookupErr == nil && num != "" {
			return num, url, false, nil
		}
	}

	// Try to find a PR for the branch.
	num, url, err := findPRForBranch(ctx, r, in.Worktree, in.Repo, in.Branch)
	if err == nil && num != "" {
		return num, url, false, nil
	}

	// Create a draft PR.
	title := titleFromPrompt(in.Prompt, in.RunID)
	body := pauseBody(in)
	args := []string{"pr", "create", "--draft", "--title", title, "--body", body, "--head", in.Branch}
	if in.Repo != "" {
		args = []string{"pr", "create", "--repo", in.Repo, "--draft", "--title", title, "--body", body, "--head", in.Branch}
	}
	stdout, err := r.GH(ctx, in.Worktree, args...)
	if err != nil {
		return "", "", false, err
	}
	url = strings.TrimSpace(stdout)
	// gh pr create prints the URL on success.
	num = extractPRNumberFromURL(url)
	return num, url, true, nil
}

func lookupPR(ctx context.Context, r Runner, workdir, repo, prNumber string) (string, string, error) {
	args := []string{"pr", "view", prNumber, "--json", "number,url", "-q", ".number,.url"}
	if repo != "" {
		args = []string{"pr", "view", prNumber, "--repo", repo, "--json", "number,url", "-q", ".number,.url"}
	}
	stdout, err := r.GH(ctx, workdir, args...)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected gh pr view output: %q", stdout)
	}
	return parts[0], parts[1], nil
}

func findPRForBranch(ctx context.Context, r Runner, workdir, repo, branch string) (string, string, error) {
	args := []string{"pr", "list", "--head", branch, "--state", "open", "--json", "number,url", "-q", ".[0].number,.[0].url"}
	if repo != "" {
		args = []string{"pr", "list", "--repo", repo, "--head", branch, "--state", "open", "--json", "number,url", "-q", ".[0].number,.[0].url"}
	}
	stdout, err := r.GH(ctx, workdir, args...)
	if err != nil {
		return "", "", err
	}
	out := strings.TrimSpace(stdout)
	if out == "" {
		return "", "", nil
	}
	parts := strings.Split(out, "\n")
	if len(parts) != 2 || parts[0] == "" {
		return "", "", nil
	}
	return parts[0], parts[1], nil
}

func applyLabel(ctx context.Context, r Runner, workdir, repo, prNumber string) error {
	args := []string{"pr", "edit", prNumber, "--add-label", event.BudgetPausedLabel}
	if repo != "" {
		args = []string{"pr", "edit", prNumber, "--repo", repo, "--add-label", event.BudgetPausedLabel}
	}
	_, err := r.GH(ctx, workdir, args...)
	return err
}

func postComment(ctx context.Context, r Runner, in PauseInput, prNumber string) error {
	body := fmt.Sprintf(
		"Agent paused at budget cap ($%.2f of $%.2f). Label `%s` set; latest commit is WIP. Use `klaus launch --pr %s \"continue the work\"` to continue, or close to abandon.",
		in.CostUSD, in.BudgetUSD, event.BudgetPausedLabel, prNumber,
	)
	args := []string{"pr", "comment", prNumber, "--body", body}
	if in.Repo != "" {
		args = []string{"pr", "comment", prNumber, "--repo", in.Repo, "--body", body}
	}
	_, err := r.GH(ctx, in.Worktree, args...)
	return err
}

func titleFromPrompt(prompt, runID string) string {
	first := strings.TrimSpace(prompt)
	if idx := strings.IndexByte(first, '\n'); idx >= 0 {
		first = first[:idx]
	}
	first = strings.TrimSpace(first)
	if first == "" {
		return fmt.Sprintf("klaus run %s (budget paused)", runID)
	}
	if len(first) > 72 {
		first = strings.TrimSpace(first[:72])
	}
	return "[budget-paused] " + first
}

func pauseBody(in PauseInput) string {
	return fmt.Sprintf(
		"This PR was opened by klaus when the agent for run `%s` exhausted its budget cap.\n\n"+
			"- **Cost so far:** $%.2f of $%.2f\n"+
			"- **Branch:** `%s`\n"+
			"- **Status:** draft, labeled `%s`\n\n"+
			"## Original prompt\n\n%s\n\n"+
			"## Continue the work\n\n"+
			"Run `klaus launch --pr <number> \"continue the work\"` to dispatch a fresh agent against this branch. "+
			"The new agent will see the WIP commit and pick up from there.\n\n"+
			"To abandon, close this PR.\n",
		in.RunID, in.CostUSD, in.BudgetUSD, in.Branch, event.BudgetPausedLabel, summarizePrompt(in.Prompt),
	)
}

func summarizePrompt(prompt string) string {
	s := strings.TrimSpace(prompt)
	if s == "" {
		return "(no prompt recorded)"
	}
	if len(s) > 500 {
		return s[:500] + "…"
	}
	return s
}

func extractPRNumberFromURL(url string) string {
	idx := strings.LastIndex(url, "/pull/")
	if idx < 0 {
		return ""
	}
	tail := url[idx+len("/pull/"):]
	end := strings.IndexAny(tail, "/?# \n\t")
	if end < 0 {
		return strings.TrimSpace(tail)
	}
	return strings.TrimSpace(tail[:end])
}

// BudgetExhausted reports whether the observed cost is close enough to the
// budget cap to treat as exhaustion. The heuristic is 95% of cap; callers
// pair this with a check of claude's result event subtype to avoid false
// positives (e.g. a successful completion that happened to land near cap).
func BudgetExhausted(cost, cap float64) bool {
	if cap <= 0 {
		return false
	}
	return cost >= 0.95*cap
}

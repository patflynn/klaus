package cmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

// mergeRunner holds the dependencies for the merge workflow.
// Fields are functions to allow testing with mocks.
type mergeRunner struct {
	out                 io.Writer
	in                  io.Reader
	getPRTitle          func(string, string) string
	getPRCI             func(string, string) string
	getPRConflicts      func(string, string) string
	getPRBehind         func(string, string) int
	getPRReviewDecision func(string, string) string
	rebaseAndPush       func(string, string) error
	mergePR             func(string, string, bool, string) error
	updateBranch        func(string, string) error
	pollCI              func(string, string) error
	markMerged          func(prNumber string)
	resolveRepo         func(prNumber string) string
	checkApproval       func(prNumber string) bool
	forceApproval       bool
	yesFlag             bool
}

func newMergeRunner(out io.Writer, in io.Reader, store run.StateStore, repoFlag string) *mergeRunner {
	ctx := context.TODO()
	r := &mergeRunner{
		out: out,
		in:  in,
		getPRTitle: func(pr, repo string) string {
			return gh.NewGHCLIClient(repo).GetTitle(ctx, pr)
		},
		getPRCI: func(pr, repo string) string {
			return gh.NewGHCLIClient(repo).GetCI(ctx, pr)
		},
		getPRConflicts: func(pr, repo string) string {
			return gh.NewGHCLIClient(repo).GetConflicts(ctx, pr)
		},
		getPRBehind: func(pr, repo string) int {
			return gh.NewGHCLIClient(repo).GetCommitsBehind(ctx, pr)
		},
		getPRReviewDecision: func(pr, repo string) string {
			return gh.NewGHCLIClient(repo).GetReviewDecision(ctx, pr)
		},
		rebaseAndPush: rebaseAndPush,
		mergePR: func(prNumber, mergeMethod string, deleteBranch bool, repo string) error {
			return gh.NewGHCLIClient(repo).Merge(ctx, prNumber, mergeMethod, deleteBranch)
		},
		updateBranch: func(prNumber, repo string) error {
			return gh.NewGHCLIClient(repo).UpdateBranch(ctx, prNumber)
		},
		pollCI:        defaultPollCI,
		markMerged:    markRunsMerged(store),
		checkApproval: buildApprovalChecker(store),
	}
	r.resolveRepo = buildRepoResolver(store, repoFlag)
	return r
}

// buildRepoResolver returns a function that resolves the target repo for a given PR number.
// Priority: run state pr_url match > --repo flag > session target > "" (existing behavior).
func buildRepoResolver(store run.StateStore, repoFlag string) func(string) string {
	// Pre-load states and session target once from the provided store.
	var states []*run.State
	if store != nil {
		states, _ = store.List()
	}

	var sessionTarget string
	if store != nil {
		sessionTarget, _ = run.LoadTarget(filepath.Dir(store.StateDir()))
	}

	return func(prNumber string) string {
		// 1. Check run states for matching pr_url
		for _, s := range states {
			if extractPRNumber(s) == prNumber && s.PRURL != nil {
				if repo := repoFromPRURL(*s.PRURL); repo != "" && repo != "(unknown)" {
					return repo
				}
			}
		}
		// 2. --repo flag
		if repoFlag != "" {
			return repoFlag
		}
		// 3. Session target
		if sessionTarget != "" {
			return sessionTarget
		}
		// 4. Empty string — gh will use the current git repo
		return ""
	}
}

// buildApprovalChecker returns a function that checks if a PR number
// has been approved in the run state or via GitHub review. Returns true if approved.
func buildApprovalChecker(store run.StateStore) func(string) bool {
	return func(prNumber string) bool {
		// Check internal approval state first.
		var states []*run.State
		if store != nil {
			states, _ = store.List()
		}

		repo := ""
		isApproved := false
		for _, s := range states {
			if extractPRNumber(s) == prNumber {
				if s.Approved != nil && *s.Approved {
					isApproved = true
				}
				if repo == "" && s.PRURL != nil {
					repo = repoFromPRURL(*s.PRURL)
					if repo == "(unknown)" {
						repo = ""
					}
				}
			}
		}

		if isApproved {
			return true
		}

		// Fall back to checking GitHub review decision.
		decision := gh.NewGHCLIClient(repo).GetReviewDecision(context.TODO(), prNumber)
		if strings.EqualFold(decision, "APPROVED") {
			// Persist the approval into run state so future checks are fast.
			for _, s := range states {
				if extractPRNumber(s) == prNumber {
					markApproved(s, store)
					break
				}
			}
			return true
		}
		return false
	}
}

// markRunsMerged returns a function that finds run states matching a PR number
// and updates their MergedAt field. This triggers the dashboard's fsnotify
// watcher so it can reflect the merge immediately.
func markRunsMerged(store run.StateStore) func(string) {
	return func(prNumber string) {
		if store == nil {
			return
		}
		states, err := store.List()
		if err != nil {
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		for _, s := range states {
			if extractPRNumber(s) == prNumber {
				s.MergedAt = &now
				if err := store.Save(s); err != nil {
					slog.Warn("failed to save merged state", "id", s.ID, "err", err)
				}
			}
		}
	}
}

var mergeCmd = &cobra.Command{
	Use:   "merge <pr1> <pr2> ...",
	Short: "Merge PRs sequentially with automatic rebasing",
	Long: `Merges a list of PRs in the given order. For each PR:

1. Resolves the target repo (from run state, --repo flag, or session target)
2. Checks merge readiness (CI, conflicts, review approval)
3. If conflicts exist, rebases onto main and re-pushes
4. Merges with the specified method (default: squash)
5. Moves to the next PR

A branch that is merely behind main is merged as-is: rebasing it would rewrite
history and dismiss the approval the merge depends on. If GitHub refuses the
merge because the branch must be up to date, klaus runs GitHub's "Update
branch" (a merge from main, not a rebase), waits for CI, and merges again.

If a rebase fails or CI times out, stops and reports the stuck PR.

Use --repo to specify the target repository when running outside a git repo
(e.g. from a klaus session workspace). If PRs were created by klaus agents,
the repo is auto-detected from run state. Rebasing needs a checkout: klaus
uses the current repo, a registered project, or clones under
worktree_base/.repos, so merge does not have to run inside a clone.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		mergeMethod, _ := cmd.Flags().GetString("merge-method")
		noDeleteBranch, _ := cmd.Flags().GetBool("no-delete-branch")
		repoFlag, _ := cmd.Flags().GetString("repo")
		force, _ := cmd.Flags().GetBool("force")
		yes, _ := cmd.Flags().GetBool("yes")

		if err := validateMergeMethod(mergeMethod); err != nil {
			return err
		}

		// Best-effort: get the session store so we can update run states
		// after merge. If not in a session, store will be nil and
		// markMerged will be a no-op.
		store, _ := sessionStore()

		runner := newMergeRunner(os.Stdout, os.Stdin, store, repoFlag)
		runner.forceApproval = force
		runner.yesFlag = yes

		// Load config (require_approval, merge_verify_command) from the target
		// repo rather than the current directory, so merging another repo's PRs
		// from a klaus session workspace still honours that repo's settings.
		// existingRepoDir never clones: when the repo has no local checkout,
		// repoDir is empty and only global config applies.
		repoDir := existingRepoDir(runner.resolveRepo(args[0]))
		cfg, err := config.Load(repoDir)
		if err != nil {
			return fmt.Errorf("could not load configuration: %w", err)
		}
		if !cfg.RequiresApproval() {
			runner.forceApproval = true // approval not required by config
		}

		if dryRun {
			return runner.dryRun(args)
		}
		return runner.run(args, mergeMethod, !noDeleteBranch)
	},
}

func validateMergeMethod(method string) error {
	switch method {
	case "squash", "merge", "rebase":
		return nil
	default:
		return fmt.Errorf("invalid merge method %q: must be squash, merge, or rebase", method)
	}
}

// rebaseAndPush rebases a PR branch onto origin/main, verifies compilation,
// and force-pushes using a temporary worktree.
func rebaseAndPush(prNumber string, repo string) error {
	ctx := context.TODO()
	branch, err := gh.NewGHCLIClient(repo).GetBranch(ctx, prNumber)
	if err != nil {
		return fmt.Errorf("getting branch: %w", err)
	}

	// The rebase needs a real checkout to build a worktree from. Resolve it
	// from the PR's repo (current checkout, registered project, or a clone
	// under worktree_base/.repos) so merge works outside a git directory.
	repoRoot, err := resolveRepoDir(ctx, repo)
	if err != nil {
		return fmt.Errorf("could not find a checkout to rebase in: %w", err)
	}

	gitClient := git.NewExecClient()

	if err := gitClient.FetchBranch(ctx, repoRoot, "main"); err != nil {
		return fmt.Errorf("fetching main: %w", err)
	}
	if err := gitClient.FetchBranch(ctx, repoRoot, branch); err != nil {
		return fmt.Errorf("fetching %s: %w", branch, err)
	}

	tmpDir, err := os.MkdirTemp("", "klaus-merge-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	worktreePath := filepath.Join(tmpDir, "rebase")
	defer func() {
		if err := gitClient.WorktreeRemove(ctx, repoRoot, worktreePath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove worktree: %v\n", err)
		}
		if err := os.RemoveAll(tmpDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove temp directory: %v\n", err)
		}
	}()

	if err := gitClient.WorktreeAddTrack(ctx, repoRoot, worktreePath, branch); err != nil {
		return fmt.Errorf("creating worktree: %w", err)
	}

	rebaseCmd := exec.Command("git", "rebase", "origin/main")
	rebaseCmd.Dir = worktreePath
	var stderr bytes.Buffer
	rebaseCmd.Stderr = &stderr
	if err := rebaseCmd.Run(); err != nil {
		abortCmd := exec.Command("git", "rebase", "--abort")
		abortCmd.Dir = worktreePath
		if abortErr := abortCmd.Run(); abortErr != nil {
			slog.Warn("failed to abort rebase", "pr", prNumber, "worktree", worktreePath, "err", abortErr)
		}
		return fmt.Errorf("rebase conflicts: %s", strings.TrimSpace(stderr.String()))
	}

	cfg, _ := config.Load(repoRoot)
	if err := verifyRebasedWorktree(worktreePath, cfg.MergeVerifyCmd()); err != nil {
		return err
	}

	pushCmd := exec.Command("git", "push", "--force-with-lease")
	pushCmd.Dir = worktreePath
	var pushStderr bytes.Buffer
	pushCmd.Stderr = &pushStderr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("force push failed: %s", strings.TrimSpace(pushStderr.String()))
	}

	return nil
}

// verifyRebasedWorktree sanity-checks the rebased branch before force-push.
// Precedence: configured merge_verify_command (via sh -c) > `go build ./...`
// iff a go.mod exists > skip (non-Go repo with no command — rely on CI).
func verifyRebasedWorktree(worktreePath string, verifyCmd *string) error {
	var cmd *exec.Cmd
	switch {
	case verifyCmd != nil:
		cmd = exec.Command("sh", "-c", *verifyCmd)
	case fileExists(filepath.Join(worktreePath, "go.mod")):
		cmd = exec.Command("go", "build", "./...")
	default:
		return nil
	}
	cmd.Dir = worktreePath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("merge verification failed after rebase: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// defaultPollCI polls CI checks until they pass or timeout.
func defaultPollCI(prNumber string, repo string) error {
	timeout := 10 * time.Minute
	interval := 30 * time.Second
	deadline := time.Now().Add(timeout)
	client := gh.NewGHCLIClient(repo)
	ctx := context.TODO()

	for {
		ci := client.GetCI(ctx, prNumber)
		switch ci {
		case "passing":
			return nil
		case "failing":
			return fmt.Errorf("CI checks failed")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("CI timed out after %v", timeout)
		}
		time.Sleep(interval)
	}
}

// formatRepoLabel returns the repo string for display, defaulting to "(local)".
func formatRepoLabel(repo string) string {
	if repo == "" {
		return "(local)"
	}
	return repo
}

// dryRun prints the merge plan without executing.
func (r *mergeRunner) dryRun(prNumbers []string) error {
	fmt.Fprintf(r.out, "Merge plan (dry run):\n\n")
	for i, prNum := range prNumbers {
		repo := r.resolveRepo(prNum)
		title := r.getPRTitle(prNum, repo)
		ci := r.getPRCI(prNum, repo)
		conflicts := r.getPRConflicts(prNum, repo)
		behind := r.getPRBehind(prNum, repo)
		review := r.getPRReviewDecision(prNum, repo)
		status := computeMergeStatus(ci, conflicts, review, behind)

		repoLabel := formatRepoLabel(repo)
		fmt.Fprintf(r.out, "  %d. PR #%s [%s]: %s\n", i+1, prNum, repoLabel, title)
		fmt.Fprintf(r.out, "     CI: %s | Conflicts: %s | Behind: %d | Review: %s | Merge: %s\n",
			ci, conflicts, behind, review, status)
	}
	return nil
}

// run merges PRs sequentially.
func (r *mergeRunner) run(prNumbers []string, mergeMethod string, deleteBranch bool) error {
	scanner := bufio.NewScanner(r.in)
	for i, prNum := range prNumbers {
		repo := r.resolveRepo(prNum)
		repoLabel := formatRepoLabel(repo)
		fmt.Fprintf(r.out, "\n[%d/%d] PR #%s [%s]\n", i+1, len(prNumbers), prNum, repoLabel)

		title := r.getPRTitle(prNum, repo)
		fmt.Fprintf(r.out, "  Title: %s\n", title)

		ci := r.getPRCI(prNum, repo)
		conflicts := r.getPRConflicts(prNum, repo)
		behind := r.getPRBehind(prNum, repo)
		review := r.getPRReviewDecision(prNum, repo)

		fmt.Fprintf(r.out, "  CI: %s | Conflicts: %s | Behind: %d | Review: %s\n",
			ci, conflicts, behind, review)

		// Check approval gate
		if !r.forceApproval && r.checkApproval != nil && !r.checkApproval(prNum) {
			if r.yesFlag {
				fmt.Fprintf(r.out, "  Skipping PR #%s: not approved\n", prNum)
				continue
			}
			// Interactive prompt
			fmt.Fprintf(r.out, "  PR #%s is not approved. Approve and merge? [y/n/s(kip)] ", prNum)
			if scanner.Scan() {
				answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
				switch answer {
				case "y", "yes":
					// Continue with merge
				case "s", "skip":
					fmt.Fprintf(r.out, "  Skipped PR #%s\n", prNum)
					continue
				default:
					return r.stopQueue(prNum, "not approved", prNumbers[i+1:])
				}
			} else {
				return r.stopQueue(prNum, "merge not confirmed", prNumbers[i+1:])
			}
		}

		// Unfixable blocker: changes requested
		if strings.EqualFold(review, "CHANGES_REQUESTED") {
			return r.stopQueue(prNum, "changes requested in review", prNumbers[i+1:])
		}

		// Conflicts are the only reason to rewrite history. The rebase
		// force-push creates a new head, and under a policy that dismisses
		// reviews on new commits that costs the PR the very approval the merge
		// depends on — so a branch that is merely behind main is left alone.
		// GitHub merges a behind branch as-is unless the repo requires branches
		// to be up to date; that refusal is handled below by a server-side
		// branch update, which preserves approvals where policy allows.
		// CI-failing-without-conflicts stops first: don't rebase a broken branch.
		if conflicts == "yes" {
			fmt.Fprintf(r.out, "  Rebasing onto main...\n")
			if err := r.rebaseAndPush(prNum, repo); err != nil {
				return r.stopQueue(prNum, fmt.Sprintf("rebase failed: %v", err), prNumbers[i+1:])
			}
			behind = 0 // the rebased head sits on top of main
			fmt.Fprintf(r.out, "  Waiting for CI after rebase...\n")
			if err := r.pollCI(prNum, repo); err != nil {
				return r.stopQueue(prNum, fmt.Sprintf("CI after rebase: %v", err), prNumbers[i+1:])
			}
		} else if ci == "failing" {
			// CI failing without conflicts — can't fix automatically
			return r.stopQueue(prNum, "CI is failing", prNumbers[i+1:])
		} else if ci != "passing" {
			// CI pending or unknown — wait
			fmt.Fprintf(r.out, "  Waiting for CI...\n")
			if err := r.pollCI(prNum, repo); err != nil {
				return r.stopQueue(prNum, fmt.Sprintf("CI: %v", err), prNumbers[i+1:])
			}
		}

		if err := r.mergeWithBranchUpdate(prNum, mergeMethod, deleteBranch, repo, behind); err != nil {
			return r.stopQueue(prNum, fmt.Sprintf("merge failed: %v", err), prNumbers[i+1:])
		}
		fmt.Fprintf(r.out, "  Merged PR #%s.\n", prNum)
		if r.markMerged != nil {
			r.markMerged(prNum)
		}
	}

	fmt.Fprintf(r.out, "\nAll %d PRs merged successfully.\n", len(prNumbers))
	return nil
}

// mergeWithBranchUpdate merges a PR, retrying once through GitHub's
// "Update branch" when the merge is refused because the branch is behind its
// base. The update merges main into the head server-side, so unlike a rebase
// and force-push it does not rewrite history — which is what lets an existing
// approval survive on repos that dismiss reviews for rewritten heads.
func (r *mergeRunner) mergeWithBranchUpdate(prNum, mergeMethod string, deleteBranch bool, repo string, behind int) error {
	fmt.Fprintf(r.out, "  Merging (%s)...\n", mergeMethod)
	err := r.mergePR(prNum, mergeMethod, deleteBranch, repo)
	if err == nil {
		return nil
	}
	// Only worth retrying when the branch really is behind: an update-branch
	// call on an up-to-date PR is rejected, and the original refusal is the
	// more useful error to report.
	if behind <= 0 || r.updateBranch == nil || !mergeRefusedForStaleBranch(err) {
		return err
	}

	fmt.Fprintf(r.out, "  Merge refused while %d commit(s) behind main; updating branch from main...\n", behind)
	if updateErr := r.updateBranch(prNum, repo); updateErr != nil {
		return fmt.Errorf("%w (updating branch from main also failed: %v)", err, updateErr)
	}
	fmt.Fprintf(r.out, "  Waiting for CI after branch update...\n")
	if ciErr := r.pollCI(prNum, repo); ciErr != nil {
		return fmt.Errorf("CI after branch update: %w", ciErr)
	}
	fmt.Fprintf(r.out, "  Merging (%s)...\n", mergeMethod)
	return r.mergePR(prNum, mergeMethod, deleteBranch, repo)
}

// mergeRefusedForStaleBranch reports whether a merge failure looks like one an
// "Update branch" would clear. GitHub does not always name the stale branch: a
// PR blocked by "require branches to be up to date" surfaces through gh as the
// same generic base-branch-policy message as any other blocked merge, so that
// text is matched too. Callers gate on the PR actually being behind, which is
// what keeps the generic match from firing on unrelated blocks.
func mergeRefusedForStaleBranch(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, phrase := range []string{
		"not up to date",
		"out of date",
		"base branch policy prohibits the merge",
		"base branch was modified",
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// stopQueue reports which PR is stuck and lists remaining unmerged PRs.
func (r *mergeRunner) stopQueue(stuckPR, reason string, remaining []string) error {
	fmt.Fprintf(r.out, "\nStopped: PR #%s — %s\n", stuckPR, reason)
	if len(remaining) > 0 {
		fmt.Fprintf(r.out, "Remaining PRs: %s\n", formatPRList(remaining))
	}
	return fmt.Errorf("PR #%s: %s", stuckPR, reason)
}

// formatPRList formats a list of PR numbers for display.
func formatPRList(prs []string) string {
	formatted := make([]string, len(prs))
	for i, pr := range prs {
		formatted[i] = "#" + pr
	}
	return strings.Join(formatted, ", ")
}

func init() {
	mergeCmd.Flags().Bool("dry-run", false, "Print the merge plan without executing")
	mergeCmd.Flags().String("merge-method", "squash", "Merge method: squash, merge, or rebase")
	mergeCmd.Flags().Bool("no-delete-branch", false, "Skip --delete-branch on gh pr merge")
	mergeCmd.Flags().String("repo", "", "Default target repo (owner/repo) for all PRs")
	mergeCmd.Flags().Bool("force", false, "Bypass approval check")
	mergeCmd.Flags().BoolP("yes", "y", false, "Skip interactive prompts (skips unapproved PRs with a warning)")
	rootCmd.AddCommand(mergeCmd)
}

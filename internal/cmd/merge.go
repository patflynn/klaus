package cmd

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
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
	getPRReviewDecision func(string, string) string
	rebaseAndPush       func(string, string) error
	mergePR             func(string, string, bool, string) error
	pollCI              func(string, string) error
	markMerged          func(prNumber string)
	resolveRepo         func(prNumber string) string
	checkApproval       func(prNumber string) bool
	forceApproval       bool
	yesFlag             bool
}

func newMergeRunner(out io.Writer, in io.Reader, store run.StateStore, repoFlag string) *mergeRunner {
	r := &mergeRunner{
		out: out,
		in:  in,
		getPRTitle: func(pr, repo string) string {
			return getPRTitle(pr, repo)
		},
		getPRCI: func(pr, repo string) string {
			return getPRCI(pr, repo)
		},
		getPRConflicts: func(pr, repo string) string {
			return getPRConflicts(pr, repo)
		},
		getPRReviewDecision: func(pr, repo string) string {
			return getPRReviewDecision(pr, repo)
		},
		rebaseAndPush: rebaseAndPush,
		mergePR:       mergePRExec,
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
	// Pre-load states and session target once.
	var states []*run.State
	if store != nil {
		states, _ = store.List()
	}
	if len(states) == 0 {
		states, _, _ = listStatesFromEnvOrAll()
	}

	var sessionTarget string
	if s, err := sessionStore(); err == nil {
		if hds, ok := s.(*run.HomeDirStore); ok {
			sessionTarget, _ = run.LoadTarget(hds.BaseDir())
		}
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
// has been approved in the run state. Returns true if approved.
func buildApprovalChecker(store run.StateStore) func(string) bool {
	return func(prNumber string) bool {
		if store == nil {
			// Try scanning all sessions
			states, _, err := listStatesFromEnvOrAll()
			if err != nil {
				return false
			}
			for _, s := range states {
				if extractPRNumber(s) == prNumber && s.Approved != nil && *s.Approved {
					return true
				}
			}
			return false
		}
		states, err := store.List()
		if err != nil {
			return false
		}
		for _, s := range states {
			if extractPRNumber(s) == prNumber && s.Approved != nil && *s.Approved {
				return true
			}
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
				store.Save(s)
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

If a rebase fails or CI times out, stops and reports the stuck PR.

Use --repo to specify the target repository when running outside a git repo
(e.g. from a klaus session workspace). If PRs were created by klaus agents,
the repo is auto-detected from run state.`,
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

		// Load config to check require_approval setting
		repoRoot, _ := git.RepoRoot()
		cfg, err := config.Load(repoRoot)
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

// ghPRTitleArgs returns arguments for fetching a PR title.
func ghPRTitleArgs(prNumber string, repo ...string) []string {
	args := []string{"pr", "view", "--json", "title", "-q", ".title"}
	if len(repo) > 0 && repo[0] != "" {
		args = append(args, "--repo", repo[0])
	}
	args = append(args, "--", prNumber)
	return args
}

// getPRTitle fetches the title of a PR using the gh CLI.
func getPRTitle(prNumber string, repo ...string) string {
	cmd := exec.Command("gh", ghPRTitleArgs(prNumber, repo...)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to get title for PR #%s: %v\n", prNumber, err)
		return "(unknown)"
	}
	title := strings.TrimSpace(stdout.String())
	if title == "" {
		return "(unknown)"
	}
	return title
}

// ghPRMergeArgs returns arguments for merging a PR.
func ghPRMergeArgs(prNumber, mergeMethod string, deleteBranch bool, repo string) []string {
	args := []string{"pr", "merge"}
	switch mergeMethod {
	case "squash":
		args = append(args, "--squash")
	case "merge":
		args = append(args, "--merge")
	case "rebase":
		args = append(args, "--rebase")
	}
	if deleteBranch {
		args = append(args, "--delete-branch")
	}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	args = append(args, "--", prNumber)
	return args
}

// mergePRExec merges a PR using the gh CLI.
func mergePRExec(prNumber, mergeMethod string, deleteBranch bool, repo string) error {
	args := ghPRMergeArgs(prNumber, mergeMethod, deleteBranch, repo)
	cmd := exec.Command("gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr merge: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// rebaseAndPush rebases a PR branch onto origin/main, verifies compilation,
// and force-pushes using a temporary worktree.
func rebaseAndPush(prNumber string, repo string) error {
	branch, err := getPRBranch(prNumber, repo)
	if err != nil {
		return fmt.Errorf("getting branch: %w", err)
	}

	repoRoot, err := git.RepoRoot()
	if err != nil {
		return fmt.Errorf("could not determine git repository root: %w", err)
	}

	if err := git.FetchBranch(repoRoot, "main"); err != nil {
		return fmt.Errorf("fetching main: %w", err)
	}
	if err := git.FetchBranch(repoRoot, branch); err != nil {
		return fmt.Errorf("fetching %s: %w", branch, err)
	}

	tmpDir, err := os.MkdirTemp("", "klaus-merge-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	worktreePath := filepath.Join(tmpDir, "rebase")
	defer func() {
		if err := git.WorktreeRemove(repoRoot, worktreePath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove worktree: %v\n", err)
		}
		if err := os.RemoveAll(tmpDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove temp directory: %v\n", err)
		}
	}()

	if err := git.WorktreeAddTrack(repoRoot, worktreePath, branch); err != nil {
		return fmt.Errorf("creating worktree: %w", err)
	}

	rebaseCmd := exec.Command("git", "rebase", "origin/main")
	rebaseCmd.Dir = worktreePath
	var stderr bytes.Buffer
	rebaseCmd.Stderr = &stderr
	if err := rebaseCmd.Run(); err != nil {
		abortCmd := exec.Command("git", "rebase", "--abort")
		abortCmd.Dir = worktreePath
		abortCmd.Run()
		return fmt.Errorf("rebase conflicts: %s", strings.TrimSpace(stderr.String()))
	}

	buildCmd := exec.Command("go", "build", "./...")
	buildCmd.Dir = worktreePath
	var buildStderr bytes.Buffer
	buildCmd.Stderr = &buildStderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("build failed after rebase: %s", strings.TrimSpace(buildStderr.String()))
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

// defaultPollCI polls CI checks until they pass or timeout.
func defaultPollCI(prNumber string, repo string) error {
	timeout := 10 * time.Minute
	interval := 30 * time.Second
	deadline := time.Now().Add(timeout)

	for {
		ci := getPRCI(prNumber, repo)
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
		review := r.getPRReviewDecision(prNum, repo)
		status := computeMergeStatus(ci, conflicts, review)

		repoLabel := formatRepoLabel(repo)
		fmt.Fprintf(r.out, "  %d. PR #%s [%s]: %s\n", i+1, prNum, repoLabel, title)
		fmt.Fprintf(r.out, "     CI: %s | Conflicts: %s | Review: %s | Merge: %s\n",
			ci, conflicts, review, status)
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
		review := r.getPRReviewDecision(prNum, repo)

		fmt.Fprintf(r.out, "  CI: %s | Conflicts: %s | Review: %s\n",
			ci, conflicts, review)

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

		// Handle conflicts via rebase
		if conflicts == "yes" {
			fmt.Fprintf(r.out, "  Rebasing onto main...\n")
			if err := r.rebaseAndPush(prNum, repo); err != nil {
				return r.stopQueue(prNum, fmt.Sprintf("rebase failed: %v", err), prNumbers[i+1:])
			}
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

		fmt.Fprintf(r.out, "  Merging (%s)...\n", mergeMethod)
		if err := r.mergePR(prNum, mergeMethod, deleteBranch, repo); err != nil {
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

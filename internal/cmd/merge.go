package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/git"
	"github.com/spf13/cobra"
)

// mergeRunner holds the dependencies for the merge workflow.
// Fields are functions to allow testing with mocks.
type mergeRunner struct {
	out                 io.Writer
	getPRTitle          func(string) string
	getPRCI             func(string) string
	getPRConflicts      func(string) string
	getPRReviewDecision func(string) string
	rebaseAndPush       func(string) error
	mergePR             func(string, string, bool) error
	pollCI              func(string) error
}

func newMergeRunner(out io.Writer) *mergeRunner {
	return &mergeRunner{
		out:                 out,
		getPRTitle:          getPRTitle,
		getPRCI:             getPRCI,
		getPRConflicts:      getPRConflicts,
		getPRReviewDecision: getPRReviewDecision,
		rebaseAndPush:       rebaseAndPush,
		mergePR:             mergePRExec,
		pollCI:              defaultPollCI,
	}
}

var mergeCmd = &cobra.Command{
	Use:   "merge <pr1> <pr2> ...",
	Short: "Merge PRs sequentially with automatic rebasing",
	Long: `Merges a list of PRs in the given order. For each PR:

1. Checks merge readiness (CI, conflicts, review approval)
2. If conflicts exist, rebases onto main and re-pushes
3. Merges with the specified method (default: squash)
4. Moves to the next PR

If a rebase fails or CI times out, stops and reports the stuck PR.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		mergeMethod, _ := cmd.Flags().GetString("merge-method")
		noDeleteBranch, _ := cmd.Flags().GetBool("no-delete-branch")

		if err := validateMergeMethod(mergeMethod); err != nil {
			return err
		}

		runner := newMergeRunner(os.Stdout)

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
func ghPRTitleArgs(prNumber string) []string {
	return []string{"pr", "view", "--json", "title", "-q", ".title", "--", prNumber}
}

// getPRTitle fetches the title of a PR using the gh CLI.
func getPRTitle(prNumber string) string {
	cmd := exec.Command("gh", ghPRTitleArgs(prNumber)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "(unknown)"
	}
	title := strings.TrimSpace(stdout.String())
	if title == "" {
		return "(unknown)"
	}
	return title
}

// ghPRMergeArgs returns arguments for merging a PR.
func ghPRMergeArgs(prNumber, mergeMethod string, deleteBranch bool) []string {
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
	args = append(args, "--", prNumber)
	return args
}

// mergePRExec merges a PR using the gh CLI.
func mergePRExec(prNumber, mergeMethod string, deleteBranch bool) error {
	args := ghPRMergeArgs(prNumber, mergeMethod, deleteBranch)
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
func rebaseAndPush(prNumber string) error {
	branch, err := getPRBranch(prNumber)
	if err != nil {
		return fmt.Errorf("getting branch: %w", err)
	}

	repoRoot, err := git.RepoRoot()
	if err != nil {
		return fmt.Errorf("not inside a git repository")
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
		git.WorktreeRemove(repoRoot, worktreePath)
		os.RemoveAll(tmpDir)
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
func defaultPollCI(prNumber string) error {
	timeout := 10 * time.Minute
	interval := 30 * time.Second
	deadline := time.Now().Add(timeout)

	for {
		ci := getPRCI(prNumber)
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

// dryRun prints the merge plan without executing.
func (r *mergeRunner) dryRun(prNumbers []string) error {
	fmt.Fprintf(r.out, "Merge plan (dry run):\n\n")
	for i, prNum := range prNumbers {
		title := r.getPRTitle(prNum)
		ci := r.getPRCI(prNum)
		conflicts := r.getPRConflicts(prNum)
		review := r.getPRReviewDecision(prNum)
		status := computeMergeStatus(ci, conflicts, review)

		fmt.Fprintf(r.out, "  %d. PR #%s: %s\n", i+1, prNum, title)
		fmt.Fprintf(r.out, "     CI: %s | Conflicts: %s | Review: %s | Merge: %s\n",
			ci, conflicts, review, status)
	}
	return nil
}

// run merges PRs sequentially.
func (r *mergeRunner) run(prNumbers []string, mergeMethod string, deleteBranch bool) error {
	for i, prNum := range prNumbers {
		fmt.Fprintf(r.out, "\n[%d/%d] PR #%s\n", i+1, len(prNumbers), prNum)

		title := r.getPRTitle(prNum)
		fmt.Fprintf(r.out, "  Title: %s\n", title)

		ci := r.getPRCI(prNum)
		conflicts := r.getPRConflicts(prNum)
		review := r.getPRReviewDecision(prNum)

		fmt.Fprintf(r.out, "  CI: %s | Conflicts: %s | Review: %s\n",
			ci, conflicts, review)

		// Unfixable blocker: changes requested
		if strings.EqualFold(review, "CHANGES_REQUESTED") {
			return r.stopQueue(prNum, "changes requested in review", prNumbers[i+1:])
		}

		// Handle conflicts via rebase
		if conflicts == "yes" {
			fmt.Fprintf(r.out, "  Rebasing onto main...\n")
			if err := r.rebaseAndPush(prNum); err != nil {
				return r.stopQueue(prNum, fmt.Sprintf("rebase failed: %v", err), prNumbers[i+1:])
			}
			fmt.Fprintf(r.out, "  Waiting for CI after rebase...\n")
			if err := r.pollCI(prNum); err != nil {
				return r.stopQueue(prNum, fmt.Sprintf("CI after rebase: %v", err), prNumbers[i+1:])
			}
		} else if ci == "failing" {
			// CI failing without conflicts — can't fix automatically
			return r.stopQueue(prNum, "CI is failing", prNumbers[i+1:])
		} else if ci != "passing" {
			// CI pending or unknown — wait
			fmt.Fprintf(r.out, "  Waiting for CI...\n")
			if err := r.pollCI(prNum); err != nil {
				return r.stopQueue(prNum, fmt.Sprintf("CI: %v", err), prNumbers[i+1:])
			}
		}

		fmt.Fprintf(r.out, "  Merging (%s)...\n", mergeMethod)
		if err := r.mergePR(prNum, mergeMethod, deleteBranch); err != nil {
			return r.stopQueue(prNum, fmt.Sprintf("merge failed: %v", err), prNumbers[i+1:])
		}
		fmt.Fprintf(r.out, "  Merged PR #%s.\n", prNum)
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
	rootCmd.AddCommand(mergeCmd)
}

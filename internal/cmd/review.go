package cmd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/review"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

var reviewCmd = &cobra.Command{
	Use:   "review <pr-ref>",
	Short: "Get a second opinion on a PR from a different model family",
	Long: `Reviews an open PR with a model family other than the one that wrote it.

The PR author's backend comes from the klaus run that opened the PR (claude when
no run is found). The reviewer is --backend, else backends.<author>.review_backend,
else the first cross_review.order entry that is not the author and whose CLI is
on PATH. klaus refuses to review a PR with its own family unless you pass both
--backend and --allow-same-family. agy cannot review yet: it has no read-only
mode (pending #307), so it is skipped in cross_review.order and refused otherwise.

The reviewer sees only the diff (gh pr diff) plus the PR title and description,
and runs read-only in an empty temp directory (claude: Read/Grep/Glob tools
only; codex: read-only sandbox, no user config); no checkout is needed.

With --post (default: cross_review.post), findings are submitted as ONE GitHub
review with event COMMENT: findings on diff lines become inline comments, the
rest go in the body with the verdict. Reviews are second opinions for the
operator, never approvals. Posting is refused when this backend already
reviewed the PR's head commit or cross_review.max_rounds reviews exist.

Accepts PR numbers, full GitHub PR URLs, or owner/repo#number references. For a
bare number the repo is --repo, else the repo of the klaus run that opened the
PR, else the session target, else the current checkout.

Examples:
  klaus review 303
  klaus review 303 --repo patflynn/klaus --backend codex
  klaus review patflynn/klaus#303 --post`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		repoFlag, _ := cmd.Flags().GetString("repo")
		backendFlag, _ := cmd.Flags().GetString("backend")
		modelFlag, _ := cmd.Flags().GetString("model")
		allowSame, _ := cmd.Flags().GetBool("allow-same-family")

		repoRef, prNumber, err := parsePRRef(args[0])
		if err != nil {
			return err
		}
		cmd.SilenceUsage = true // args are valid; later errors are not usage errors
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		var states []*run.State
		store, err := sessionStore() // best effort: no session → author claude
		if err == nil {
			states, _ = store.List()
		}
		if repoRef == "" {
			repoRef = repoFlag
		}
		if repoRef == "" {
			repoRef = buildRepoResolver(store, "")(prNumber)
		}
		ownerRepo, err := reviewRepoSlug(ctx, repoRef)
		if err != nil {
			return err
		}

		cfg, err := config.Load(existingRepoDir(ownerRepo))
		if err != nil {
			return fmt.Errorf("could not load configuration: %w", err)
		}
		author := prAuthorBackend(states, ownerRepo, prNumber)

		var reviewer backend.Kind
		var model string
		if backendFlag != "" {
			if reviewer, err = backend.Parse(backendFlag); err != nil {
				return err
			}
			if err := review.CheckReviewer(reviewer); err != nil {
				return err
			}
			model = review.ReviewModel(reviewer, cfg)
		} else if reviewer, model, err = review.ChooseReviewer(author, cfg); err != nil {
			return err
		}
		if modelFlag != "" {
			model = modelFlag
		}
		if reviewer == author && backendFlag == "" {
			return fmt.Errorf("PR #%s was written by %s and no other reviewer family is available: install another backend CLI, set cross_review.order or backends.%s.review_backend, or pass --backend %s --allow-same-family", prNumber, author, author, author)
		}
		if reviewer == author && !allowSame {
			return fmt.Errorf("PR #%s was written by %s; refusing a same-family review: pick another --backend or add --allow-same-family", prNumber, author)
		}

		post := cfg.CrossReviewPost()
		if cmd.Flags().Changed("post") {
			post, _ = cmd.Flags().GetBool("post")
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Cross-model review of %s#%s (author: %s; reviewer: %s %s)\n", ownerRepo, prNumber, author, reviewer, modelLabel(model))
		result, err := review.RunPRReview(ctx, ownerRepo, prNumber, review.PROptions{
			Backend: reviewer, Model: model, Post: post, MaxRounds: cfg.CrossReviewMaxRounds(),
			LockDir: reviewLockDir(store),
		})
		if result != nil {
			printPRReview(out, result)
		}
		if err != nil {
			return err
		}
		if result.ReviewURL != "" {
			fmt.Fprintf(out, "\nPosted COMMENT review: %s\n", result.ReviewURL)
		} else if post {
			fmt.Fprintln(out, "\nNothing posted: no changes to review.")
		}
		return nil
	},
}

// reviewLockDir: <session>/locks, else ~/.klaus/locks when no session exists.
func reviewLockDir(store run.StateStore) string {
	if hds, ok := store.(*run.HomeDirStore); ok {
		return filepath.Join(hds.BaseDir(), "locks")
	}
	if dir, err := run.SessionsDir(); err == nil {
		return filepath.Join(filepath.Dir(dir), "locks")
	}
	return ""
}

// reviewRepoSlug normalizes owner/repo, URLs, and registered project names to owner/repo; "" → the current checkout's repo.
func reviewRepoSlug(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		owner, repo, err := github.NewGHCLIClient("").GetRepoOwnerAndName(ctx)
		if err != nil {
			return "", fmt.Errorf("no repo given (use --repo owner/repo): %w", err)
		}
		return owner + "/" + repo, nil
	}
	if slug := repoSlug(ref); slug != "" {
		return slug, nil
	}
	if slug := repoSlugForDir(existingRepoDir(ref)); slug != "" {
		return slug, nil
	}
	return "", fmt.Errorf("cannot resolve repo %q to owner/repo", ref)
}

// prAuthorBackend returns the backend of the earliest run whose PR URL is ownerRepo#prNumber; claude when none.
func prAuthorBackend(states []*run.State, ownerRepo, prNumber string) backend.Kind {
	var matches []*run.State
	for _, s := range states {
		if s.PRURL != nil && extractPRNumber(s) == prNumber && strings.EqualFold(github.OwnerRepoFromPRURL(*s.PRURL), ownerRepo) {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return backend.Claude
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].CreatedAt < matches[j].CreatedAt })
	k, err := backend.Parse(matches[0].Backend)
	if err != nil {
		return backend.Claude
	}
	return k
}

func printPRReview(w io.Writer, r *review.ReviewResult) {
	if r.Verdict != "" {
		fmt.Fprintf(w, "\nVerdict: %s\n", r.Verdict)
	}
	fmt.Fprintln(w, "\nFindings:")
	printFindings(w, r.Findings)
	if r.Summary != "" {
		fmt.Fprintf(w, "\nSummary: %s\n", r.Summary)
	}
}

func init() {
	reviewCmd.Flags().String("repo", "", "Target repo (owner/repo or registered project) for a bare PR number")
	reviewCmd.Flags().String("backend", "", "Reviewer backend: claude or codex; agy pending #307 (default: cross-review choice)")
	reviewCmd.Flags().String("model", "", "Reviewer model (default: backends.<reviewer>.review_model)")
	reviewCmd.Flags().Bool("post", false, "Submit findings as one COMMENT review on the PR (default: cross_review.post)")
	reviewCmd.Flags().Bool("allow-same-family", false, "With --backend, allow reviewing with the PR author's own family")
	rootCmd.AddCommand(reviewCmd)
}

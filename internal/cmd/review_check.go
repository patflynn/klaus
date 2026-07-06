package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/github"
)

// ghReview represents a single review from the GitHub API.
type ghReview struct {
	ID          int64  `json:"id"`
	User        ghUser `json:"user"`
	State       string `json:"state"`
	SubmittedAt string `json:"submitted_at"`
}

type ghUser struct {
	Login string `json:"login"`
}

// ghReviewComment represents an inline review comment from the GitHub
// pulls comments API. Only the parent review ID is needed here.
type ghReviewComment struct {
	PullRequestReviewID int64 `json:"pull_request_review_id"`
}

// ghCommit represents a commit from the GitHub pulls commits API.
type ghCommit struct {
	Commit ghCommitDetail `json:"commit"`
}

type ghCommitDetail struct {
	Committer ghCommitActor `json:"committer"`
}

type ghCommitActor struct {
	Date string `json:"date"`
}

// hasUnaddressedTrustedComments checks whether a PR has inline review
// comments from trusted reviewers that haven't been addressed by a
// subsequent push.
func hasUnaddressedTrustedComments(ownerRepo, prNumber string) bool {
	cfg, err := config.Load("")
	if err != nil || len(cfg.TrustedReviewers) == 0 {
		return false
	}

	trustedSet := make(map[string]bool, len(cfg.TrustedReviewers))
	for _, r := range cfg.TrustedReviewers {
		trustedSet[r] = true
	}

	// Fetch reviews.
	reviews := fetchPRReviews(ownerRepo, prNumber)
	if len(reviews) == 0 {
		return false
	}

	// Collect trusted reviewer reviews with state COMMENTED or CHANGES_REQUESTED.
	var candidates []ghReview
	for _, r := range reviews {
		if !trustedSet[r.User.Login] {
			continue
		}
		state := strings.ToUpper(r.State)
		if state != "COMMENTED" && state != "CHANGES_REQUESTED" {
			continue
		}
		candidates = append(candidates, r)
	}
	if len(candidates) == 0 {
		return false
	}

	// Only reviews with at least one inline comment are actionable. A
	// body-only review (e.g. gemini-code-assist's summary COMMENTED review)
	// gives a fix agent nothing to address: the agent correctly pushes no
	// commit, so the commit-time watermark below never advances and the
	// review would read as "unaddressed" forever, redispatching fix agents
	// in a loop. Body-only CHANGES_REQUESTED reviews still flip GitHub's
	// reviewDecision and are handled by the changes-requested path instead.
	reviewsWithInline := fetchReviewIDsWithInlineComments(ownerRepo, prNumber)

	// Find the most recent actionable trusted review.
	var latestTrustedReviewTime time.Time
	for _, r := range candidates {
		if !reviewsWithInline[r.ID] {
			continue
		}
		t, err := time.Parse(time.RFC3339, r.SubmittedAt)
		if err != nil {
			continue
		}
		if t.After(latestTrustedReviewTime) {
			latestTrustedReviewTime = t
		}
	}

	if latestTrustedReviewTime.IsZero() {
		return false
	}

	// Fetch the latest commit timestamp.
	latestCommitTime := fetchLatestCommitTime(ownerRepo, prNumber)
	if latestCommitTime.IsZero() {
		// Can't determine commit time; assume comments are unaddressed.
		return true
	}

	// Comments are unaddressed if the latest trusted review is after the latest commit.
	return latestTrustedReviewTime.After(latestCommitTime)
}

// fetchPRReviews calls gh api to get reviews for a PR.
func fetchPRReviews(ownerRepo, prNumber string) []ghReview {
	client := github.NewGHCLIClient("")
	endpoint := "repos/" + ownerRepo + "/pulls/" + prNumber + "/reviews"
	out, err := client.APIGet(context.TODO(), endpoint)
	if err != nil {
		return nil
	}
	var reviews []ghReview
	if err := json.Unmarshal(out, &reviews); err != nil {
		return nil
	}
	return reviews
}

// fetchReviewIDsWithInlineComments calls gh api to get the inline review
// comments on a PR and returns the set of review IDs that own at least one.
// On error it returns nil (no review counts), which fails toward "no dispatch".
func fetchReviewIDsWithInlineComments(ownerRepo, prNumber string) map[int64]bool {
	client := github.NewGHCLIClient("")
	endpoint := "repos/" + ownerRepo + "/pulls/" + prNumber + "/comments"
	out, err := client.APIGet(context.TODO(), endpoint)
	if err != nil {
		return nil
	}
	var comments []ghReviewComment
	if err := json.Unmarshal(out, &comments); err != nil {
		return nil
	}
	ids := make(map[int64]bool, len(comments))
	for _, c := range comments {
		if c.PullRequestReviewID != 0 {
			ids[c.PullRequestReviewID] = true
		}
	}
	return ids
}

// fetchLatestCommitTime calls gh api to get the latest commit time on a PR.
// It uses the committer date, not the author date: author dates survive
// rebases unchanged and can be arbitrarily old, which would falsely mark a
// fresh push as predating the reviews it addresses.
func fetchLatestCommitTime(ownerRepo, prNumber string) time.Time {
	client := github.NewGHCLIClient("")
	endpoint := "repos/" + ownerRepo + "/pulls/" + prNumber + "/commits"
	out, err := client.APIGet(context.TODO(), endpoint)
	if err != nil {
		return time.Time{}
	}
	var commits []ghCommit
	if err := json.Unmarshal(out, &commits); err != nil {
		return time.Time{}
	}
	var latest time.Time
	for _, c := range commits {
		t, err := time.Parse(time.RFC3339, c.Commit.Committer.Date)
		if err != nil {
			continue
		}
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

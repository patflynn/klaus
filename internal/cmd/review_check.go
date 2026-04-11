package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/github"
)

// ownerRepoFromPRURL extracts "owner/repo" from a full GitHub PR URL.
func ownerRepoFromPRURL(prURL string) string {
	prURL = strings.TrimPrefix(prURL, "https://github.com/")
	prURL = strings.TrimPrefix(prURL, "http://github.com/")
	parts := strings.Split(prURL, "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return ""
}

// ghReview represents a single review from the GitHub API.
type ghReview struct {
	User        ghUser `json:"user"`
	State       string `json:"state"`
	SubmittedAt string `json:"submitted_at"`
}

type ghUser struct {
	Login string `json:"login"`
}

// ghCommit represents a commit from the GitHub pulls commits API.
type ghCommit struct {
	Commit ghCommitDetail `json:"commit"`
}

type ghCommitDetail struct {
	Author ghCommitAuthor `json:"author"`
}

type ghCommitAuthor struct {
	Date string `json:"date"`
}

// hasUnaddressedTrustedComments checks whether a PR has review comments from
// trusted reviewers that haven't been addressed by a subsequent push.
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

	// Find the most recent trusted reviewer review with state COMMENTED or CHANGES_REQUESTED.
	var latestTrustedReviewTime time.Time
	for _, r := range reviews {
		if !trustedSet[r.User.Login] {
			continue
		}
		state := strings.ToUpper(r.State)
		if state != "COMMENTED" && state != "CHANGES_REQUESTED" {
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

// fetchLatestCommitTime calls gh api to get the latest commit time on a PR.
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
		t, err := time.Parse(time.RFC3339, c.Commit.Author.Date)
		if err != nil {
			continue
		}
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

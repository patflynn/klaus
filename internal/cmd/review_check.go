package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/pipeline"
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
// pulls comments API. Only the parent review ID and body are needed here.
type ghReviewComment struct {
	PullRequestReviewID int64  `json:"pull_request_review_id"`
	Body                string `json:"body"`
}

// ghIssueComment represents a PR conversation comment from the GitHub issues
// comments API (a PR is an issue, so its top-level thread lives there).
type ghIssueComment struct {
	User      ghUser `json:"user"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// isAgentReply reports whether a comment body was posted by a klaus fix agent.
// Agents run gh as the operator, who is often a trusted reviewer, so the
// author login can't tell their replies apart; the prompt has them tag each
// reply with pipeline.AgentReplyMarker instead. Counting those replies would
// make every agent reply look like fresh trusted feedback and redispatch.
func isAgentReply(body string) bool {
	return strings.Contains(body, pipeline.AgentReplyMarker)
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

// hasUnaddressedTrustedComments checks whether a PR has comments from trusted
// reviewers that haven't been addressed by a subsequent push. Both inline
// review comments and PR conversation comments count; the PR author's own
// comments are not treated specially — only trusted_reviewers membership and
// the agent-reply marker decide whether a comment counts.
func hasUnaddressedTrustedComments(ownerRepo, prNumber string) bool {
	cfg, err := config.Load("")
	if err != nil || len(cfg.TrustedReviewers) == 0 {
		return false
	}

	trustedSet := make(map[string]bool, len(cfg.TrustedReviewers))
	for _, r := range cfg.TrustedReviewers {
		trustedSet[r] = true
	}

	latestTrustedCommentTime := latestTrustedReviewTime(ownerRepo, prNumber, trustedSet)
	if t := latestTrustedConversationCommentTime(ownerRepo, prNumber, trustedSet); t.After(latestTrustedCommentTime) {
		latestTrustedCommentTime = t
	}
	if latestTrustedCommentTime.IsZero() {
		return false
	}

	// Fetch the latest commit timestamp.
	latestCommitTime := fetchLatestCommitTime(ownerRepo, prNumber)
	if latestCommitTime.IsZero() {
		// Can't determine commit time; assume comments are unaddressed.
		return true
	}

	// Comments are unaddressed if the latest trusted comment is after the latest commit.
	return latestTrustedCommentTime.After(latestCommitTime)
}

// latestTrustedReviewTime returns the submission time of the most recent
// actionable review from a trusted reviewer, or the zero time if there is none.
func latestTrustedReviewTime(ownerRepo, prNumber string, trustedSet map[string]bool) time.Time {
	reviews := fetchPRReviews(ownerRepo, prNumber)
	if len(reviews) == 0 {
		return time.Time{}
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
		return time.Time{}
	}

	// Only reviews with at least one inline comment are actionable. A
	// body-only review (e.g. gemini-code-assist's summary COMMENTED review)
	// gives a fix agent nothing to address: the agent correctly pushes no
	// commit, so the commit-time watermark never advances and the review
	// would read as "unaddressed" forever, redispatching fix agents in a
	// loop. Body-only CHANGES_REQUESTED reviews still flip GitHub's
	// reviewDecision and are handled by the changes-requested path instead.
	reviewsWithInline := fetchReviewIDsWithInlineComments(ownerRepo, prNumber)

	var latest time.Time
	for _, r := range candidates {
		if !reviewsWithInline[r.ID] {
			continue
		}
		t, err := time.Parse(time.RFC3339, r.SubmittedAt)
		if err != nil {
			continue
		}
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

// latestTrustedConversationCommentTime returns the creation time of the most
// recent PR conversation comment from a trusted reviewer, ignoring fix-agent
// replies, or the zero time if there is none.
func latestTrustedConversationCommentTime(ownerRepo, prNumber string, trustedSet map[string]bool) time.Time {
	var latest time.Time
	for _, c := range fetchPRConversationComments(ownerRepo, prNumber) {
		if !trustedSet[c.User.Login] || isAgentReply(c.Body) {
			continue
		}
		t, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil {
			continue
		}
		if t.After(latest) {
			latest = t
		}
	}
	return latest
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
// comments on a PR and returns the set of review IDs that own at least one
// comment not written by a fix agent. A reply posted through the replies
// endpoint creates its own review, so without that exclusion an agent's reply
// would make the review read as fresh trusted feedback. On error it returns nil (no review counts), which fails toward "no dispatch".
// per_page=100 raises the default page size of 30, which would otherwise drop
// newer reviews' comments on busy PRs and misread them as body-only.
func fetchReviewIDsWithInlineComments(ownerRepo, prNumber string) map[int64]bool {
	client := github.NewGHCLIClient("")
	endpoint := "repos/" + ownerRepo + "/pulls/" + prNumber + "/comments?per_page=100"
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
		if c.PullRequestReviewID != 0 && !isAgentReply(c.Body) {
			ids[c.PullRequestReviewID] = true
		}
	}
	return ids
}

// fetchPRConversationComments calls gh api to get the PR conversation
// comments, which GitHub serves from the issues API. On error it returns nil,
// which fails toward "no dispatch". per_page=100 raises the default page size
// of 30 so newer comments on busy PRs stay in view (results are oldest-first).
func fetchPRConversationComments(ownerRepo, prNumber string) []ghIssueComment {
	client := github.NewGHCLIClient("")
	endpoint := "repos/" + ownerRepo + "/issues/" + prNumber + "/comments?per_page=100"
	out, err := client.APIGet(context.TODO(), endpoint)
	if err != nil {
		return nil
	}
	var comments []ghIssueComment
	if err := json.Unmarshal(out, &comments); err != nil {
		return nil
	}
	return comments
}

// fetchLatestCommitTime calls gh api to get the latest commit time on a PR.
// It uses the committer date, not the author date: author dates survive
// rebases unchanged and can be arbitrarily old, which would falsely mark a
// fresh push as predating the reviews it addresses. The endpoint returns
// commits oldest-first with a default page size of 30, so per_page=100 keeps
// the newest commit in view on PRs with more than 30 commits.
func fetchLatestCommitTime(ownerRepo, prNumber string) time.Time {
	client := github.NewGHCLIClient("")
	endpoint := "repos/" + ownerRepo + "/pulls/" + prNumber + "/commits?per_page=100"
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

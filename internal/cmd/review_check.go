package cmd

import (
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/pipeline"
)

// isAgentReply reports whether a comment body was posted by a klaus fix agent.
// Agents run gh as the operator, who is often a trusted reviewer, so the
// author login can't tell their replies apart; the prompt has them tag each
// reply with pipeline.AgentReplyMarker instead. Counting those replies would
// make every agent reply look like fresh trusted feedback and redispatch.
func isAgentReply(body string) bool {
	return strings.Contains(body, pipeline.AgentReplyMarker)
}

// isOptedIn reports whether a conversation comment explicitly asks for a fix:
// its first non-blank line is pipeline.FixCommand (optionally followed by
// text), or some line is exactly pipeline.ActionableMarker. Both must stand on
// their own line, so a reply that quotes an opted-in comment ("> /klaus fix")
// doesn't inherit the opt-in.
func isOptedIn(body string) bool {
	first := true
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == pipeline.ActionableMarker {
			return true
		}
		if first {
			first = false
			if rest, ok := strings.CutPrefix(line, pipeline.FixCommand); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
				return true
			}
		}
	}
	return false
}

// hasUnaddressedTrustedComments reports whether a PR has comments from
// trusted reviewers that no push has addressed since. Both inline review
// comments and PR conversation comments count. Conversation comments by the
// PR author count only when opted in (see isOptedIn), because the operator
// and fix agents share that account; inline review comments don't treat the
// author specially.
func hasUnaddressedTrustedComments(a *github.PRActivity, trustedReviewers []string) bool {
	_, unaddressed := trustedFeedback(a, trustedReviewers)
	return unaddressed
}

// trustedFeedback returns when trusted reviewers last left actionable
// feedback on the PR (zero when never), and whether no push has addressed it
// since.
func trustedFeedback(a *github.PRActivity, trustedReviewers []string) (time.Time, bool) {
	if a == nil || len(trustedReviewers) == 0 {
		return time.Time{}, false
	}
	trustedSet := make(map[string]bool, len(trustedReviewers))
	for _, r := range trustedReviewers {
		trustedSet[r] = true
	}

	latest := latestTrustedReviewTime(a, trustedSet)
	if t := latestTrustedConversationCommentTime(a, trustedSet); t.After(latest) {
		latest = t
	}
	if latest.IsZero() {
		return latest, false
	}
	if a.LastCommitAt.IsZero() {
		// Can't determine commit time; assume comments are unaddressed.
		return latest, true
	}
	// The head's committer date, not its author date: author dates survive
	// rebases unchanged and can be arbitrarily old, which would falsely mark
	// a fresh push as predating the reviews it addresses.
	return latest, latest.After(a.LastCommitAt)
}

// latestTrustedReviewTime returns the submission time of the most recent
// actionable review from a trusted reviewer, or the zero time if there is none.
//
// A `klaus review --post` review is trusted when the operator's gh account
// posted it; the marker alone is not, or anyone could trigger fix agents.
//
// Only reviews with at least one inline comment not written by a fix agent
// are actionable. A body-only review (e.g. gemini-code-assist's summary
// COMMENTED review) gives a fix agent nothing to address: the agent correctly
// pushes no commit, so the commit-time watermark never advances and the review
// would read as "unaddressed" forever, redispatching fix agents in a loop.
// Body-only CHANGES_REQUESTED reviews still flip GitHub's reviewDecision and
// are handled by the changes-requested path instead. A reply posted through
// the replies endpoint creates its own review, so an agent's reply must not
// make its review count.
func latestTrustedReviewTime(a *github.PRActivity, trustedSet map[string]bool) time.Time {
	var latest time.Time
	for _, r := range a.Reviews {
		if !trustedSet[r.Author] {
			if !strings.Contains(r.Body, pipeline.CrossReviewMarker) || a.Operator == "" || r.Author != a.Operator {
				continue
			}
		}
		state := strings.ToUpper(r.State)
		if state != "COMMENTED" && state != "CHANGES_REQUESTED" {
			continue
		}
		inline := false
		for _, body := range r.Comments {
			if !isAgentReply(body) {
				inline = true
				break
			}
		}
		if inline && r.SubmittedAt.After(latest) {
			latest = r.SubmittedAt
		}
	}
	return latest
}

// latestTrustedConversationCommentTime returns the last-updated time of the
// most recent actionable PR conversation comment from a trusted reviewer, or
// the zero time if there is none.
//
// A comment by the PR author is actionable only when opted in: the operator
// and fix agents post through the same account, so an agent reply that omits
// or mangles pipeline.AgentReplyMarker must not read as fresh feedback. The
// marker check stays as a second guard. If the PR author isn't known, every
// comment is held to the opt-in rule, failing toward "no dispatch".
//
// The last edit is the watermark, so a reviewer editing an older comment
// after the latest push to add feedback is picked up.
func latestTrustedConversationCommentTime(a *github.PRActivity, trustedSet map[string]bool) time.Time {
	var latest time.Time
	for _, c := range a.Comments {
		if !trustedSet[c.Author] || isAgentReply(c.Body) {
			continue
		}
		if (a.Author == "" || c.Author == a.Author) && !isOptedIn(c.Body) {
			continue
		}
		if c.UpdatedAt.After(latest) {
			latest = c.UpdatedAt
		}
	}
	return latest
}

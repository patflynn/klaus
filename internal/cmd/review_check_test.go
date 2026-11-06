package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/github"

	"github.com/patflynn/klaus/internal/pipeline"
)

// activityFixture builds the review activity hasUnaddressedTrustedComments
// reads from REST-shaped JSON: reviews, their inline comments (by review
// ID), conversation comments and commits. The PR author is "patflynn", who
// is not trusted by the default config, which trusts gemini-code-assist[bot].
func activityFixture(t *testing.T, reviews, comments, commits string) *github.PRActivity {
	t.Helper()
	return activityFixtureWithConversation(t, reviews, comments, `[]`, commits)
}

// activityFixtureWithConversation is activityFixture plus conversation comments.
func activityFixtureWithConversation(t *testing.T, reviews, comments, conversation, commits string) *github.PRActivity {
	t.Helper()
	return activityFixtureWithAuthor(t, "patflynn", reviews, comments, conversation, commits)
}

// activityFixtureWithAuthor is activityFixtureWithConversation with the given
// PR author, who is also the operator gh runs as.
func activityFixtureWithAuthor(t *testing.T, author, reviews, comments, conversation, commits string) *github.PRActivity {
	t.Helper()
	type user struct {
		Login string `json:"login"`
	}
	var rs []struct {
		ID          int64  `json:"id"`
		User        user   `json:"user"`
		State       string `json:"state"`
		SubmittedAt string `json:"submitted_at"`
		Body        string `json:"body"`
	}
	var cs []struct {
		ReviewID int64  `json:"pull_request_review_id"`
		Body     string `json:"body"`
	}
	var conv []struct {
		User      user   `json:"user"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	var commitList []struct {
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	for _, j := range []struct {
		in  string
		out any
	}{{reviews, &rs}, {comments, &cs}, {conversation, &conv}, {commits, &commitList}} {
		if err := json.Unmarshal([]byte(j.in), j.out); err != nil {
			t.Fatalf("fixture %s: %v", j.in, err)
		}
	}
	parse := func(s string) time.Time {
		ts, _ := time.Parse(time.RFC3339, s)
		return ts
	}
	a := &github.PRActivity{Operator: author, Author: author}
	for _, r := range rs {
		rv := github.Review{ID: r.ID, Author: r.User.Login, State: r.State, Body: r.Body, SubmittedAt: parse(r.SubmittedAt)}
		for _, c := range cs {
			if c.ReviewID == r.ID {
				rv.Comments = append(rv.Comments, c.Body)
			}
		}
		a.Reviews = append(a.Reviews, rv)
	}
	for _, c := range conv {
		at := c.UpdatedAt
		if at == "" {
			at = c.CreatedAt
		}
		a.Comments = append(a.Comments, github.IssueComment{Author: c.User.Login, Body: c.Body, UpdatedAt: parse(at)})
	}
	for _, c := range commitList {
		if ts := parse(c.Commit.Committer.Date); ts.After(a.LastCommitAt) {
			a.LastCommitAt = ts
		}
	}
	return a
}

// unaddressed asks hasUnaddressedTrustedComments about a fixture with the
// default trusted reviewers.
func unaddressed(a *github.PRActivity) bool {
	return hasUnaddressedTrustedComments(a, config.Defaults().TrustedReviewers)
}

// Production repro (2026-07-06): gemini-code-assist left a COMMENTED review
// consisting of only a body — zero inline comments — on a docs PR. A fix
// agent has nothing to address and pushes no commit, so the commit-time
// watermark never advances; treating the review as "unaddressed" redispatched
// fix agents forever. Body-only reviews must not count.
func TestHasUnaddressedTrustedComments_BodyOnlyReview(t *testing.T) {
	a := activityFixture(t,
		`[{"id": 100, "user": {"login": "gemini-code-assist[bot]"}, "state": "COMMENTED", "submitted_at": "2026-07-06T10:00:00Z"}]`,
		`[]`,
		`[{"commit": {"committer": {"date": "2026-07-06T09:00:00Z"}}}]`,
	)
	if unaddressed(a) {
		t.Error("body-only trusted COMMENTED review must not count as unaddressed")
	}
}

func TestHasUnaddressedTrustedComments_InlineComments(t *testing.T) {
	a := activityFixture(t,
		`[{"id": 100, "user": {"login": "gemini-code-assist[bot]"}, "state": "COMMENTED", "submitted_at": "2026-07-06T10:00:00Z"}]`,
		`[{"pull_request_review_id": 100}]`,
		`[{"commit": {"committer": {"date": "2026-07-06T09:00:00Z"}}}]`,
	)
	if !unaddressed(a) {
		t.Error("trusted review with inline comments and no newer commit must count as unaddressed")
	}
}

func TestHasUnaddressedTrustedComments_AddressedByNewerCommit(t *testing.T) {
	a := activityFixture(t,
		`[{"id": 100, "user": {"login": "gemini-code-assist[bot]"}, "state": "COMMENTED", "submitted_at": "2026-07-06T10:00:00Z"}]`,
		`[{"pull_request_review_id": 100}]`,
		`[{"commit": {"committer": {"date": "2026-07-06T11:00:00Z"}}}]`,
	)
	if unaddressed(a) {
		t.Error("review followed by a newer commit must count as addressed")
	}
}

// A rebase preserves the author date; only the committer date reflects when
// the push actually happened. Here the addressing commit was authored before
// the review but committed after it — going by author date would falsely
// mark the review unaddressed and redispatch.
func TestHasUnaddressedTrustedComments_UsesCommitterDateNotAuthorDate(t *testing.T) {
	a := activityFixture(t,
		`[{"id": 100, "user": {"login": "gemini-code-assist[bot]"}, "state": "COMMENTED", "submitted_at": "2026-07-06T10:00:00Z"}]`,
		`[{"pull_request_review_id": 100}]`,
		`[{"commit": {"author": {"date": "2026-07-06T08:00:00Z"}, "committer": {"date": "2026-07-06T11:00:00Z"}}}]`,
	)
	if unaddressed(a) {
		t.Error("rebased commit newer than review (by committer date) must count as addressed")
	}
}

func TestHasUnaddressedTrustedComments_UntrustedReviewer(t *testing.T) {
	a := activityFixture(t,
		`[{"id": 100, "user": {"login": "drive-by-user"}, "state": "COMMENTED", "submitted_at": "2026-07-06T10:00:00Z"}]`,
		`[{"pull_request_review_id": 100}]`,
		`[{"commit": {"committer": {"date": "2026-07-06T09:00:00Z"}}}]`,
	)
	if unaddressed(a) {
		t.Error("untrusted reviewer's comments must not trigger dispatch")
	}
}

// Production repro (2026-09-14): the operator, a trusted reviewer, left review
// feedback as PR conversation comments (issues API) rather than inline review
// comments, and nothing was dispatched.
func TestHasUnaddressedTrustedComments_ConversationComment(t *testing.T) {
	a := activityFixtureWithConversation(t,
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this", "created_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if !unaddressed(a) {
		t.Error("trusted conversation comment with no newer commit must count as unaddressed")
	}
}

func TestHasUnaddressedTrustedComments_ConversationCommentAddressedByNewerCommit(t *testing.T) {
	a := activityFixtureWithConversation(t,
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this", "created_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T11:00:00Z"}}}]`,
	)
	if unaddressed(a) {
		t.Error("conversation comment followed by a newer commit must count as addressed")
	}
}

func TestHasUnaddressedTrustedComments_UntrustedConversationComment(t *testing.T) {
	a := activityFixtureWithConversation(t,
		`[]`,
		`[]`,
		`[{"user": {"login": "drive-by-user"}, "body": "please rename this", "created_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if unaddressed(a) {
		t.Error("untrusted user's conversation comment must not trigger dispatch")
	}
}

// Fix agents post as the operator, so their replies carry a trusted login and
// land after the push. Counting them would redispatch a fix agent on every
// reply it writes.
func TestHasUnaddressedTrustedComments_IgnoresAgentReplies(t *testing.T) {
	reply := `"done, renamed ` + pipeline.AgentReplyMarker + `"`
	a := activityFixtureWithConversation(t,
		`[{"id": 200, "user": {"login": "gemini-code-assist[bot]"}, "state": "COMMENTED", "submitted_at": "2026-09-14T12:00:00Z"}]`,
		`[{"pull_request_review_id": 200, "body": `+reply+`}]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this", "created_at": "2026-09-14T10:00:00Z"},
		  {"user": {"login": "gemini-code-assist[bot]"}, "body": `+reply+`, "created_at": "2026-09-14T12:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T11:00:00Z"}}}]`,
	)
	if unaddressed(a) {
		t.Error("agent replies (inline or conversation) newer than the push must not count as unaddressed")
	}
}

// The operator and fix agents post through the PR author's account, so a
// plain author conversation comment is not feedback, even when the author is
// a trusted reviewer.
func TestHasUnaddressedTrustedComments_AuthorConversationCommentIgnored(t *testing.T) {
	a := activityFixtureWithAuthor(t, "gemini-code-assist[bot]",
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "CI is green, merging once approved", "created_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if unaddressed(a) {
		t.Error("PR author's conversation comment without opt-in must not trigger dispatch")
	}
}

func TestHasUnaddressedTrustedComments_AuthorConversationCommentOptedInWithFixCommand(t *testing.T) {
	a := activityFixtureWithAuthor(t, "gemini-code-assist[bot]",
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "`+pipeline.FixCommand+`\nplease rename this", "created_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if !unaddressed(a) {
		t.Error("PR author's conversation comment opening with /klaus fix must count as unaddressed")
	}
}

func TestHasUnaddressedTrustedComments_AuthorConversationCommentOptedInWithMarker(t *testing.T) {
	a := activityFixtureWithAuthor(t, "gemini-code-assist[bot]",
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this\n`+pipeline.ActionableMarker+`", "created_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if !unaddressed(a) {
		t.Error("PR author's conversation comment carrying the actionable marker must count as unaddressed")
	}
}

// A fix agent's reply that forgets its marker lands after the push as an
// author comment. Even quoting the opted-in original must not re-arm it.
func TestHasUnaddressedTrustedComments_AgentReplyMissingMarkerIgnored(t *testing.T) {
	a := activityFixtureWithAuthor(t, "gemini-code-assist[bot]",
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "`+pipeline.FixCommand+` please rename this", "created_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:00:00Z"},
		  {"user": {"login": "gemini-code-assist[bot]"}, "body": "> `+pipeline.FixCommand+` please rename this\n> `+pipeline.ActionableMarker+`\n\nDone, renamed.", "created_at": "2026-09-14T12:00:00Z", "updated_at": "2026-09-14T12:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T11:00:00Z"}}}]`,
	)
	if unaddressed(a) {
		t.Error("agent reply without its marker must not trigger dispatch: it is an author comment without opt-in")
	}
}

func TestHasUnaddressedTrustedComments_NonAuthorTrustedConversationCommentNeedsNoOptIn(t *testing.T) {
	a := activityFixtureWithAuthor(t, "patflynn",
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this", "created_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if !unaddressed(a) {
		t.Error("trusted non-author conversation comment must count as unaddressed without opt-in")
	}
}

// A reviewer edits a comment created before the latest push to add new
// feedback: updated_at, not created_at, must be compared to the push.
func TestHasUnaddressedTrustedComments_EditedConversationCommentAfterPush(t *testing.T) {
	a := activityFixtureWithConversation(t,
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this\n\nEdit: the test helper too", "created_at": "2026-09-14T08:00:00Z", "updated_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if !unaddressed(a) {
		t.Error("conversation comment edited after the latest push must count as unaddressed")
	}
}

// `klaus review --post` posts through the operator's gh account, which need not be in trusted_reviewers.
func TestHasUnaddressedTrustedComments_OperatorCrossReview(t *testing.T) {
	for _, tt := range []struct {
		login string
		want  bool
	}{
		{login: "patflynn", want: true},       // the authenticated gh user
		{login: "drive-by-user", want: false}, // copied marker grants nothing
	} {
		t.Run(tt.login, func(t *testing.T) {
			body := `"**Cross-model review** by codex\n\n` + pipeline.CrossReviewMarker + ` backend=codex model= sha=abc -->"`
			a := activityFixtureWithAuthor(t, "patflynn",
				`[{"id": 300, "user": {"login": "`+tt.login+`"}, "state": "COMMENTED", "submitted_at": "2026-09-18T10:00:00Z", "body": `+body+`}]`,
				`[{"pull_request_review_id": 300, "body": "nil deref"}]`,
				`[]`,
				`[{"commit": {"committer": {"date": "2026-09-18T09:00:00Z"}}}]`,
			)
			if got := unaddressed(a); got != tt.want {
				t.Errorf("cross-review by %s: unaddressed = %v, want %v", tt.login, got, tt.want)
			}
		})
	}
}

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patflynn/klaus/internal/pipeline"
)

// installFakeGH puts a fake `gh` binary on PATH that serves canned JSON for
// the pulls API endpoints hasUnaddressedTrustedComments hits, so the
// real fetch + parse + decision path runs end-to-end. It also points HOME at
// an empty dir so config.Load falls back to defaults, which trust
// gemini-code-assist[bot].
func installFakeGH(t *testing.T, reviews, comments, commits string) {
	t.Helper()
	installFakeGHWithConversation(t, reviews, comments, `[]`, commits)
}

// installFakeGHWithConversation is installFakeGH plus canned PR conversation
// comments, served from the issues comments endpoint. The PR author is
// "patflynn", who is not trusted by the default config.
func installFakeGHWithConversation(t *testing.T, reviews, comments, conversation, commits string) {
	t.Helper()
	installFakeGHWithAuthor(t, "patflynn", reviews, comments, conversation, commits)
}

// installFakeGHWithAuthor is installFakeGHWithConversation with the PR author
// login served from the pulls endpoint.
func installFakeGHWithAuthor(t *testing.T, author, reviews, comments, conversation, commits string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	reviewsPath := write("reviews.json", reviews)
	commentsPath := write("comments.json", comments)
	conversationPath := write("conversation.json", conversation)
	commitsPath := write("commits.json", commits)
	prPath := write("pr.json", `{"user": {"login": "`+author+`"}}`)

	script := "#!/bin/sh\n" +
		"case \"$2\" in\n" +
		"*/reviews*) cat '" + reviewsPath + "' ;;\n" +
		"*/issues/*/comments*) cat '" + conversationPath + "' ;;\n" +
		"*/comments*) cat '" + commentsPath + "' ;;\n" +
		"*/commits*) cat '" + commitsPath + "' ;;\n" +
		"*/pulls/*) cat '" + prPath + "' ;;\n" +
		"*) echo '[]' ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
}

// Production repro (2026-07-06): gemini-code-assist left a COMMENTED review
// consisting of only a body — zero inline comments — on a docs PR. A fix
// agent has nothing to address and pushes no commit, so the commit-time
// watermark never advances; treating the review as "unaddressed" redispatched
// fix agents forever. Body-only reviews must not count.
func TestHasUnaddressedTrustedComments_BodyOnlyReview(t *testing.T) {
	installFakeGH(t,
		`[{"id": 100, "user": {"login": "gemini-code-assist[bot]"}, "state": "COMMENTED", "submitted_at": "2026-07-06T10:00:00Z"}]`,
		`[]`,
		`[{"commit": {"committer": {"date": "2026-07-06T09:00:00Z"}}}]`,
	)
	if hasUnaddressedTrustedComments("owner/repo", "42") {
		t.Error("body-only trusted COMMENTED review must not count as unaddressed")
	}
}

func TestHasUnaddressedTrustedComments_InlineComments(t *testing.T) {
	installFakeGH(t,
		`[{"id": 100, "user": {"login": "gemini-code-assist[bot]"}, "state": "COMMENTED", "submitted_at": "2026-07-06T10:00:00Z"}]`,
		`[{"pull_request_review_id": 100}]`,
		`[{"commit": {"committer": {"date": "2026-07-06T09:00:00Z"}}}]`,
	)
	if !hasUnaddressedTrustedComments("owner/repo", "42") {
		t.Error("trusted review with inline comments and no newer commit must count as unaddressed")
	}
}

func TestHasUnaddressedTrustedComments_AddressedByNewerCommit(t *testing.T) {
	installFakeGH(t,
		`[{"id": 100, "user": {"login": "gemini-code-assist[bot]"}, "state": "COMMENTED", "submitted_at": "2026-07-06T10:00:00Z"}]`,
		`[{"pull_request_review_id": 100}]`,
		`[{"commit": {"committer": {"date": "2026-07-06T11:00:00Z"}}}]`,
	)
	if hasUnaddressedTrustedComments("owner/repo", "42") {
		t.Error("review followed by a newer commit must count as addressed")
	}
}

// A rebase preserves the author date; only the committer date reflects when
// the push actually happened. Here the addressing commit was authored before
// the review but committed after it — going by author date would falsely
// mark the review unaddressed and redispatch.
func TestHasUnaddressedTrustedComments_UsesCommitterDateNotAuthorDate(t *testing.T) {
	installFakeGH(t,
		`[{"id": 100, "user": {"login": "gemini-code-assist[bot]"}, "state": "COMMENTED", "submitted_at": "2026-07-06T10:00:00Z"}]`,
		`[{"pull_request_review_id": 100}]`,
		`[{"commit": {"author": {"date": "2026-07-06T08:00:00Z"}, "committer": {"date": "2026-07-06T11:00:00Z"}}}]`,
	)
	if hasUnaddressedTrustedComments("owner/repo", "42") {
		t.Error("rebased commit newer than review (by committer date) must count as addressed")
	}
}

func TestHasUnaddressedTrustedComments_UntrustedReviewer(t *testing.T) {
	installFakeGH(t,
		`[{"id": 100, "user": {"login": "drive-by-user"}, "state": "COMMENTED", "submitted_at": "2026-07-06T10:00:00Z"}]`,
		`[{"pull_request_review_id": 100}]`,
		`[{"commit": {"committer": {"date": "2026-07-06T09:00:00Z"}}}]`,
	)
	if hasUnaddressedTrustedComments("owner/repo", "42") {
		t.Error("untrusted reviewer's comments must not trigger dispatch")
	}
}

// Production repro (2026-09-14): the operator, a trusted reviewer, left review
// feedback as PR conversation comments (issues API) rather than inline review
// comments, and nothing was dispatched.
func TestHasUnaddressedTrustedComments_ConversationComment(t *testing.T) {
	installFakeGHWithConversation(t,
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this", "created_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if !hasUnaddressedTrustedComments("owner/repo", "22") {
		t.Error("trusted conversation comment with no newer commit must count as unaddressed")
	}
}

func TestHasUnaddressedTrustedComments_ConversationCommentAddressedByNewerCommit(t *testing.T) {
	installFakeGHWithConversation(t,
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this", "created_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T11:00:00Z"}}}]`,
	)
	if hasUnaddressedTrustedComments("owner/repo", "22") {
		t.Error("conversation comment followed by a newer commit must count as addressed")
	}
}

func TestHasUnaddressedTrustedComments_UntrustedConversationComment(t *testing.T) {
	installFakeGHWithConversation(t,
		`[]`,
		`[]`,
		`[{"user": {"login": "drive-by-user"}, "body": "please rename this", "created_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if hasUnaddressedTrustedComments("owner/repo", "22") {
		t.Error("untrusted user's conversation comment must not trigger dispatch")
	}
}

// Fix agents post as the operator, so their replies carry a trusted login and
// land after the push. Counting them would redispatch a fix agent on every
// reply it writes.
func TestHasUnaddressedTrustedComments_IgnoresAgentReplies(t *testing.T) {
	reply := `"done, renamed ` + pipeline.AgentReplyMarker + `"`
	installFakeGHWithConversation(t,
		`[{"id": 200, "user": {"login": "gemini-code-assist[bot]"}, "state": "COMMENTED", "submitted_at": "2026-09-14T12:00:00Z"}]`,
		`[{"pull_request_review_id": 200, "body": `+reply+`}]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this", "created_at": "2026-09-14T10:00:00Z"},
		  {"user": {"login": "gemini-code-assist[bot]"}, "body": `+reply+`, "created_at": "2026-09-14T12:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T11:00:00Z"}}}]`,
	)
	if hasUnaddressedTrustedComments("owner/repo", "22") {
		t.Error("agent replies (inline or conversation) newer than the push must not count as unaddressed")
	}
}

// The operator and fix agents post through the PR author's account, so a
// plain author conversation comment is not feedback, even when the author is
// a trusted reviewer.
func TestHasUnaddressedTrustedComments_AuthorConversationCommentIgnored(t *testing.T) {
	installFakeGHWithAuthor(t, "gemini-code-assist[bot]",
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "CI is green, merging once approved", "created_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if hasUnaddressedTrustedComments("owner/repo", "22") {
		t.Error("PR author's conversation comment without opt-in must not trigger dispatch")
	}
}

func TestHasUnaddressedTrustedComments_AuthorConversationCommentOptedInWithFixCommand(t *testing.T) {
	installFakeGHWithAuthor(t, "gemini-code-assist[bot]",
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "`+pipeline.FixCommand+`\nplease rename this", "created_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if !hasUnaddressedTrustedComments("owner/repo", "22") {
		t.Error("PR author's conversation comment opening with /klaus fix must count as unaddressed")
	}
}

func TestHasUnaddressedTrustedComments_AuthorConversationCommentOptedInWithMarker(t *testing.T) {
	installFakeGHWithAuthor(t, "gemini-code-assist[bot]",
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this\n`+pipeline.ActionableMarker+`", "created_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if !hasUnaddressedTrustedComments("owner/repo", "22") {
		t.Error("PR author's conversation comment carrying the actionable marker must count as unaddressed")
	}
}

// A fix agent's reply that forgets its marker lands after the push as an
// author comment. Even quoting the opted-in original must not re-arm it.
func TestHasUnaddressedTrustedComments_AgentReplyMissingMarkerIgnored(t *testing.T) {
	installFakeGHWithAuthor(t, "gemini-code-assist[bot]",
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "`+pipeline.FixCommand+` please rename this", "created_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:00:00Z"},
		  {"user": {"login": "gemini-code-assist[bot]"}, "body": "> `+pipeline.FixCommand+` please rename this\n> `+pipeline.ActionableMarker+`\n\nDone, renamed.", "created_at": "2026-09-14T12:00:00Z", "updated_at": "2026-09-14T12:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T11:00:00Z"}}}]`,
	)
	if hasUnaddressedTrustedComments("owner/repo", "22") {
		t.Error("agent reply without its marker must not trigger dispatch: it is an author comment without opt-in")
	}
}

func TestHasUnaddressedTrustedComments_NonAuthorTrustedConversationCommentNeedsNoOptIn(t *testing.T) {
	installFakeGHWithAuthor(t, "patflynn",
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this", "created_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if !hasUnaddressedTrustedComments("owner/repo", "22") {
		t.Error("trusted non-author conversation comment must count as unaddressed without opt-in")
	}
}

// A reviewer edits a comment created before the latest push to add new
// feedback: updated_at, not created_at, must be compared to the push.
func TestHasUnaddressedTrustedComments_EditedConversationCommentAfterPush(t *testing.T) {
	installFakeGHWithConversation(t,
		`[]`,
		`[]`,
		`[{"user": {"login": "gemini-code-assist[bot]"}, "body": "please rename this\n\nEdit: the test helper too", "created_at": "2026-09-14T08:00:00Z", "updated_at": "2026-09-14T10:00:00Z"}]`,
		`[{"commit": {"committer": {"date": "2026-09-14T09:00:00Z"}}}]`,
	)
	if !hasUnaddressedTrustedComments("owner/repo", "22") {
		t.Error("conversation comment edited after the latest push must count as unaddressed")
	}
}

package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// installFakeGH puts a fake `gh` binary on PATH that serves canned JSON for
// the three pulls API endpoints hasUnaddressedTrustedComments hits, so the
// real fetch + parse + decision path runs end-to-end. It also points HOME at
// an empty dir so config.Load falls back to defaults, which trust
// gemini-code-assist[bot].
func installFakeGH(t *testing.T, reviews, comments, commits string) {
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
	commitsPath := write("commits.json", commits)

	script := "#!/bin/sh\n" +
		"case \"$2\" in\n" +
		"*/reviews) cat '" + reviewsPath + "' ;;\n" +
		"*/comments) cat '" + commentsPath + "' ;;\n" +
		"*/commits) cat '" + commitsPath + "' ;;\n" +
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

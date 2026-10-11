package cmd

import (
	"context"
	"strings"
	"testing"
)

func TestDefaultBranchName(t *testing.T) {
	const id = "20261011-1843-bd92f081"
	cases := []struct {
		name, id, issueTitle, prompt, want string
	}{
		{
			name:   "heading with repo prefix, cut at a word",
			prompt: "# cosmo: the klaus-env guest starts with classic-laddie\n\nBody text.\n",
			want:   "the-klaus-env-guest-starts-with-classic-bd92f081",
		},
		{
			name:   "repo prefix is case-insensitive",
			prompt: "# Cosmo: Fix the boot order",
			want:   "fix-the-boot-order-bd92f081",
		},
		{
			name:   "repo name later in the title stays",
			prompt: "# Teach cosmo: the boot order",
			want:   "teach-cosmo-the-boot-order-bd92f081",
		},
		{
			name:   "first heading beats an earlier first line",
			prompt: "Some context first.\n\n# Add a retry budget\n\n# Second heading\n",
			want:   "add-a-retry-budget-bd92f081",
		},
		{
			name:   "only level-1 headings count",
			prompt: "Fix the flaky watcher test.\n\n## Why\nIt flakes.\n",
			want:   "fix-the-flaky-watcher-test-bd92f081",
		},
		{
			name:   "a heading inside a code fence is not the title",
			prompt: "Run the installer:\n```sh\n# install deps\nmake\n```\n~~~\n# also not this\n~~~\n",
			want:   "run-the-installer-bd92f081",
		},
		{
			name:   "a heading after a closed fence counts",
			prompt: "```\n# comment\n```\n# Real title\n",
			want:   "real-title-bd92f081",
		},
		{
			name:   "first non-empty line, CRLF",
			prompt: "\r\n   \r\n  Rename the config loader  \r\nmore\r\n",
			want:   "rename-the-config-loader-bd92f081",
		},
		{
			name:   "#word is not a heading",
			prompt: "#hashtag sync fix\n",
			want:   "hashtag-sync-fix-bd92f081",
		},
		{
			name:       "issue title beats the heading",
			issueTitle: "cosmo: Dashboard crashes on resize",
			prompt:     "# Something else\n",
			want:       "dashboard-crashes-on-resize-bd92f081",
		},
		{
			name:       "issue title without a slug falls through to the heading",
			issueTitle: "🔥🔥",
			prompt:     "# Put out the fire\n",
			want:       "put-out-the-fire-bd92f081",
		},
		{
			name:   "unicode and punctuation collapse to single dashes",
			prompt: "# Café — naïve résumé: fix \"quotes\" & (parens)!!\n",
			want:   "caf-na-ve-r-sum-fix-quotes-parens-bd92f081",
		},
		{
			name:   "cut keeps a word that ends exactly at 40",
			prompt: "# " + strings.Repeat("a", 40) + " more words",
			want:   strings.Repeat("a", 40) + "-bd92f081",
		},
		{
			name:   "a single word over 40 is cut hard",
			prompt: "# " + strings.Repeat("x", 50),
			want:   strings.Repeat("x", 40) + "-bd92f081",
		},
		{
			name:   "no slug anywhere falls back",
			prompt: "# 日本語のタイトル\n本文\n",
			want:   "agent/" + id,
		},
		{
			name:   "repo prefix alone and punctuation fall back",
			prompt: "cosmo:\n",
			want:   "agent/" + id,
		},
		{
			name:   "a name git rejects falls back",
			id:     "20261011-1843-tail.lock",
			prompt: "# Fix it",
			want:   "agent/20261011-1843-tail.lock",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := tc.id
			if runID == "" {
				runID = id
			}
			got := defaultBranchName(context.Background(), runID, "cosmo", tc.issueTitle, tc.prompt)
			if got != tc.want {
				t.Errorf("defaultBranchName = %q, want %q", got, tc.want)
			}
		})
	}
}

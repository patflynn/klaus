package cmd

import (
	"context"
	"strings"

	"github.com/patflynn/klaus/internal/git"
)

// maxBranchSlug caps the descriptive part of a derived branch name.
const maxBranchSlug = 40

// defaultBranchName names a new run's branch <slug>-<tail>, where tail is the
// run ID's random tail and slug comes from the first of the issue title, the
// prompt's first "# " heading, and the prompt's first non-empty line that
// yields one. It falls back to agent/<id> when none does or git rejects the
// name.
func defaultBranchName(ctx context.Context, id, repoName, issueTitle, prompt string) string {
	heading, firstLine := briefTitles(prompt)
	for _, src := range []string{issueTitle, heading, firstLine} {
		slug := branchSlug(src, repoName)
		if slug == "" {
			continue
		}
		name := slug + "-" + id[strings.LastIndex(id, "-")+1:]
		if git.CheckBranchName(ctx, name) != nil {
			break
		}
		return name
	}
	return "agent/" + id
}

// briefTitles returns the text of the prompt's first level-1 Markdown heading
// outside fenced code blocks, and the prompt's first non-empty line.
func briefTitles(prompt string) (heading, firstLine string) {
	var fence string
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if firstLine == "" {
			firstLine = line
		}
		switch {
		case fence != "":
			if strings.HasPrefix(line, fence) {
				fence = ""
			}
		case strings.HasPrefix(line, "```"), strings.HasPrefix(line, "~~~"):
			fence = line[:3]
		case strings.HasPrefix(line, "# "), strings.HasPrefix(line, "#\t"):
			return strings.TrimSpace(line[1:]), firstLine
		}
	}
	return "", firstLine
}

// branchSlug lowercases text without a leading "<repoName>:", joins its runs
// of [a-z0-9] with single dashes, and cuts it at a word boundary to at most
// maxBranchSlug characters.
func branchSlug(text, repoName string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	if repoName != "" {
		text = strings.TrimPrefix(text, strings.ToLower(repoName)+":")
	}
	var b strings.Builder
	gap := false
	for _, r := range text {
		if !('a' <= r && r <= 'z' || '0' <= r && r <= '9') {
			gap = true
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteByte('-')
		}
		gap = false
		b.WriteRune(r)
	}
	slug := b.String()
	if len(slug) <= maxBranchSlug {
		return slug
	}
	if i := strings.LastIndexByte(slug[:maxBranchSlug+1], '-'); i > 0 {
		return slug[:i]
	}
	return slug[:maxBranchSlug]
}

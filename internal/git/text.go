package git

import (
	"strings"
	"unicode/utf8"
)

// NotText replaces the hunks of a file that isn't valid UTF-8 in TextDiff.
const NotText = "[the content of this file is not text and was left out]"

// TextDiff returns a unified diff as valid UTF-8, for a model's prompt: the
// CLIs refuse any other input. A file whose hunks aren't valid UTF-8 keeps
// its header, so the reader still sees that it changed, and has its hunks
// replaced with the line NotText.
func TextDiff(diff string) string {
	if utf8.ValidString(diff) {
		return diff
	}
	var b strings.Builder
	for _, file := range splitFiles(diff) {
		if utf8.ValidString(file) {
			b.WriteString(file)
			continue
		}
		header, hunks := file, ""
		if strings.HasPrefix(file, "@@ ") {
			header, hunks = "", file
		} else if i := strings.Index(file, "\n@@ "); i >= 0 {
			header, hunks = file[:i+1], file[i+1:]
		}
		// Only a path can make a header invalid; keep it, marked.
		b.WriteString(strings.ToValidUTF8(header, "�"))
		if utf8.ValidString(hunks) {
			b.WriteString(hunks)
		} else {
			b.WriteString(NotText + "\n")
		}
	}
	return b.String()
}

// splitFiles splits a diff before each "diff --git" line.
func splitFiles(diff string) []string {
	var files []string
	for diff != "" {
		end := strings.Index(diff, "\ndiff --git ")
		if end < 0 {
			return append(files, diff)
		}
		files = append(files, diff[:end+1])
		diff = diff[end+1:]
	}
	return files
}

// TruncateText cuts s to at most n bytes at a rune boundary, so valid UTF-8
// stays valid.
func TruncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

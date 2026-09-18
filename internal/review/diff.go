package review

import (
	"regexp"
	"strconv"
	"strings"
)

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// DiffLines maps each file in a unified diff to the new-side line numbers its hunks show (added + context): the lines GitHub accepts inline comments on with side=RIGHT.
func DiffLines(diff string) map[string]map[int]bool {
	files := map[string]map[int]bool{}
	var cur map[int]bool // nil for deleted files
	var line, oldLeft, newLeft int
	for _, l := range strings.Split(diff, "\n") {
		if oldLeft > 0 || newLeft > 0 { // inside a hunk; counts, not prefixes, mark its end
			switch {
			case strings.HasPrefix(l, "-"):
				oldLeft--
			case strings.HasPrefix(l, "+"):
				if cur != nil {
					cur[line] = true
				}
				line++
				newLeft--
			case strings.HasPrefix(l, `\`): // "\ No newline at end of file"
			default: // context; "" when trailing whitespace was stripped
				if cur != nil {
					cur[line] = true
				}
				line++
				oldLeft--
				newLeft--
			}
			continue
		}
		switch {
		case strings.HasPrefix(l, "diff --git "):
			cur = nil
		case strings.HasPrefix(l, "+++ "):
			path := strings.TrimPrefix(l, "+++ ")
			if path == "/dev/null" {
				cur = nil
				continue
			}
			if uq, err := strconv.Unquote(path); err == nil {
				path = uq
			}
			cur = map[int]bool{}
			files[strings.TrimPrefix(path, "b/")] = cur
		default:
			if m := hunkHeader.FindStringSubmatch(l); m != nil {
				oldLeft, newLeft = hunkCount(m[1]), hunkCount(m[3])
				line, _ = strconv.Atoi(m[2])
			}
		}
	}
	return files
}

func hunkCount(s string) int {
	if s == "" {
		return 1
	}
	n, _ := strconv.Atoi(s)
	return n
}

// diffPath resolves a finding's file to a key of lines, tolerating "./" and "b/" prefixes.
func diffPath(lines map[string]map[int]bool, file string) (string, bool) {
	for _, p := range []string{file, strings.TrimPrefix(file, "./"), strings.TrimPrefix(file, "b/")} {
		if _, ok := lines[p]; ok {
			return p, true
		}
	}
	return "", false
}

package github

import "strings"

// OwnerRepoFromPRURL extracts the "owner/repo" slug from a full GitHub PR URL
// such as "https://github.com/owner/repo/pull/123". It tolerates a trailing
// slash and returns "" when the URL does not contain a non-empty owner and
// repo segment (e.g. a bare short name like "klaus" or a malformed URL).
//
// This is the single source of truth for PR-URL slug parsing; callers in
// internal/cmd and internal/pipeline both delegate here rather than reimplementing
// the split.
func OwnerRepoFromPRURL(prURL string) string {
	s := strings.TrimPrefix(prURL, "https://github.com/")
	s = strings.TrimPrefix(s, "http://github.com/")
	parts := strings.Split(s, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

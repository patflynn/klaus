package project

import (
	"strings"
)

// NormalizeRepoName resolves a repo reference to its shortest canonical name
// using the project registry. The rules are:
//  1. If ref is a registered project name, return it as-is.
//  2. If ref is owner/repo (or a full GitHub URL) and the repo portion matches
//     a registered project name, return just the project name.
//  3. If ref is owner/repo that does NOT match a registered project, keep owner/repo.
//  4. If ref is empty, return empty (caller decides how to handle).
//
// Full GitHub URLs are stripped to owner/repo before checking.
func NormalizeRepoName(ref string, reg *Registry) string {
	if ref == "" {
		return ""
	}

	// Strip URL prefixes and .git suffix to get owner/repo form
	cleaned := ref
	cleaned = strings.TrimSuffix(cleaned, ".git")
	for _, prefix := range []string{
		"https://github.com/",
		"http://github.com/",
		"git@github.com:",
	} {
		if strings.HasPrefix(cleaned, prefix) {
			cleaned = strings.TrimPrefix(cleaned, prefix)
			break
		}
	}
	cleaned = strings.TrimRight(cleaned, "/")

	// If no slash, it might already be a project name
	if !strings.Contains(cleaned, "/") {
		if reg != nil {
			if _, ok := reg.Get(cleaned); ok {
				return cleaned
			}
		}
		return cleaned
	}

	// It's owner/repo — extract the repo portion
	parts := strings.SplitN(cleaned, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return cleaned
	}
	owner := parts[0]
	repo := parts[1]

	// Check if the repo name matches a registered project
	if reg != nil {
		if _, ok := reg.Get(repo); ok {
			return repo
		}
	}

	return owner + "/" + repo
}

package github

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestIsNoAccess(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"PR not found", "GraphQL: Could not resolve to a PullRequest with the number of 42. (repository.pullRequest)", true},
		{"repo not visible", "GraphQL: Could not resolve to a Repository with the name 'patflynn/cosmo'. (repository)", true},
		{"REST 404", "HTTP 404: Not Found (https://api.github.com/repos/patflynn/cosmo/pulls/42)", true},
		{"REST 403", "HTTP 403: Resource not accessible by integration (https://api.github.com/graphql)", true},
		{"rate limited 403", "HTTP 403: API rate limit exceeded for user ID 1. (https://api.github.com/graphql)", false},
		{"secondary rate limit", "HTTP 403: You have exceeded a secondary rate limit.", false},
		{"network", "error connecting to api.github.com", false},
		{"server error", "HTTP 502: Bad Gateway (https://api.github.com/graphql)", false},
		{"auth", "To get started with GitHub CLI, please run:  gh auth login", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNoAccess(tt.stderr); got != tt.want {
				t.Errorf("IsNoAccess(%q) = %v, want %v", tt.stderr, got, tt.want)
			}
		})
	}
}

// fakeGH puts a gh on PATH that prints stderr and exits with code.
func fakeGH(t *testing.T, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + stderr + "' >&2\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))
}

func TestGetStateNoAccess(t *testing.T) {
	fakeGH(t, "GraphQL: Could not resolve to a Repository with the name patflynn/cosmo. (repository)", 1)
	if got := NewGHCLIClient("").GetState(nil, "https://github.com/patflynn/cosmo/pull/42"); got != StateNoAccess {
		t.Errorf("GetState() = %q, want %q", got, StateNoAccess)
	}
}

func TestGetStateTransientFailureIsUnknown(t *testing.T) {
	fakeGH(t, "error connecting to api.github.com", 1)
	if got := NewGHCLIClient("").GetState(nil, "https://github.com/patflynn/cosmo/pull/42"); got != "UNKNOWN" {
		t.Errorf("GetState() = %q, want UNKNOWN", got)
	}
}

package github

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNewPRClient(t *testing.T) {
	c := NewPRClient("owner/repo")
	if c.Repo() != "owner/repo" {
		t.Errorf("Repo() = %q, want %q", c.Repo(), "owner/repo")
	}

	empty := NewPRClient("")
	if empty.Repo() != "" {
		t.Errorf("Repo() = %q, want empty", empty.Repo())
	}
}

func TestGHArgsWithRepo(t *testing.T) {
	c := NewPRClient("owner/repo")
	got := c.ChecksArgs("42")
	want := []string{"pr", "checks", "--repo", "owner/repo", "--", "42"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChecksArgs() = %v, want %v", got, want)
	}
}

func TestGHArgsWithoutRepo(t *testing.T) {
	c := NewPRClient("")
	got := c.ChecksArgs("42")
	want := []string{"pr", "checks", "--", "42"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChecksArgs() = %v, want %v", got, want)
	}
}

func TestAllArgBuildersPlaceFlagsBeforeSeparator(t *testing.T) {
	client := NewPRClient("owner/repo")
	builders := []struct {
		name string
		fn   func(string) []string
	}{
		{"ChecksArgs", client.ChecksArgs},
		{"ViewStateArgs", client.ViewStateArgs},
		{"ViewConflictsArgs", client.ViewConflictsArgs},
		{"ViewReviewDecisionArgs", client.ViewReviewDecisionArgs},
		{"ViewTitleArgs", client.ViewTitleArgs},
	}

	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			args := b.fn("42")

			separatorIdx := -1
			for i, arg := range args {
				if arg == "--" {
					separatorIdx = i
					break
				}
			}
			if separatorIdx == -1 {
				t.Fatal("missing '--' separator")
			}
			for i := separatorIdx + 1; i < len(args); i++ {
				if strings.HasPrefix(args[i], "-") {
					t.Errorf("flag %q found after '--' separator at index %d", args[i], i)
				}
			}

			// Last arg should be the PR ref
			last := args[len(args)-1]
			if last != "42" {
				t.Errorf("last arg = %q, want %q", last, "42")
			}
		})
	}
}

func TestMergeArgsVariants(t *testing.T) {
	tests := []struct {
		name         string
		prNumber     string
		mergeMethod  string
		deleteBranch bool
		repo         string
		want         []string
	}{
		{
			name:         "squash with delete",
			prNumber:     "42",
			mergeMethod:  "squash",
			deleteBranch: true,
			want:         []string{"pr", "merge", "--squash", "--delete-branch", "--", "42"},
		},
		{
			name:         "merge without delete",
			prNumber:     "10",
			mergeMethod:  "merge",
			deleteBranch: false,
			want:         []string{"pr", "merge", "--merge", "--", "10"},
		},
		{
			name:         "with repo",
			prNumber:     "7",
			mergeMethod:  "rebase",
			deleteBranch: true,
			repo:         "owner/repo",
			want:         []string{"pr", "merge", "--rebase", "--delete-branch", "--repo", "owner/repo", "--", "7"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeArgs(tt.prNumber, tt.mergeMethod, tt.deleteBranch, tt.repo)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MergeArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCIStatus(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "all passing",
			output: "Build\tpass\t1m30s\thttps://example.com\nLint\tpass\t30s\thttps://example.com",
			want:   "passing",
		},
		{
			name:   "one failing",
			output: "Build\tpass\t1m30s\thttps://example.com\nLint\tfail\t30s\thttps://example.com",
			want:   "failing",
		},
		{
			name:   "one pending",
			output: "Build\tpass\t1m30s\thttps://example.com\nDeploy\t\t0\thttps://example.com",
			want:   "pending",
		},
		{
			name:   "skipped checks treated as passing",
			output: "E2E Tests\tpass\t2m\thttps://example.com\nDispatch Preview Cleanup\tskipping\t0\thttps://example.com",
			want:   "passing",
		},
		{
			name:   "all skipped",
			output: "Dispatch Preview Cleanup\tskipping\t0\thttps://example.com",
			want:   "passing",
		},
		{
			name:   "skipped with failure",
			output: "Build\tfail\t1m\thttps://example.com\nCleanup\tskipping\t0\thttps://example.com",
			want:   "failing",
		},
		{
			name:   "empty output (no checks configured)",
			output: "",
			want:   "passing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCIStatus(tt.output)
			if got != tt.want {
				t.Errorf("ParseCIStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestArgsAcceptFullURL(t *testing.T) {
	url := "https://github.com/owner/repo/pull/42"
	client := NewPRClient("")

	builders := []struct {
		name string
		fn   func(string) []string
	}{
		{"ChecksArgs", client.ChecksArgs},
		{"ViewStateArgs", client.ViewStateArgs},
		{"ViewConflictsArgs", client.ViewConflictsArgs},
		{"ViewReviewDecisionArgs", client.ViewReviewDecisionArgs},
	}

	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			args := b.fn(url)
			last := args[len(args)-1]
			if last != url {
				t.Errorf("last arg = %q, want %q", last, url)
			}
		})
	}
}

func TestGetCI_NoChecksConfigured(t *testing.T) {
	// Create a fake "gh" that exits 1 with no stdout (simulates no CI checks).
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Prepend our fake gh to PATH so exec.Command("gh", ...) finds it first.
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+origPath)

	client := NewPRClient("")
	got := client.GetCI("42")
	if got != "passing" {
		t.Errorf("GetCI() with no checks configured = %q, want %q", got, "passing")
	}
}

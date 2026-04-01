package github

import (
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

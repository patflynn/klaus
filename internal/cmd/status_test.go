package cmd

import (
	"reflect"
	"testing"

	"github.com/patflynn/klaus/internal/run"
)

func TestComputeMergeStatus(t *testing.T) {
	tests := []struct {
		name           string
		ci             string
		conflicts      string
		reviewDecision string
		want           string
	}{
		{"all green approved", "passing", "none", "APPROVED", "ready"},
		{"all green no review", "passing", "none", "", "ready"},
		{"ci failing", "failing", "none", "APPROVED", "blocked"},
		{"conflicts", "passing", "yes", "APPROVED", "blocked"},
		{"changes requested", "passing", "none", "CHANGES_REQUESTED", "blocked"},
		{"ci pending", "pending", "none", "APPROVED", "pending"},
		{"review unknown", "passing", "none", "unknown", "pending"},
		{"ci failing and conflicts", "failing", "yes", "", "blocked"},
		{"changes requested case insensitive", "passing", "none", "changes_requested", "blocked"},
		{"unknown CI unknown review", "unknown", "none", "unknown", "pending"},
		{"unknown CI no conflicts", "unknown", "none", "", "pending"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeMergeStatus(tt.ci, tt.conflicts, tt.reviewDecision)
			if got != tt.want {
				t.Errorf("computeMergeStatus(%q, %q, %q) = %q, want %q",
					tt.ci, tt.conflicts, tt.reviewDecision, got, tt.want)
			}
		})
	}
}

func TestExtractPRNumber(t *testing.T) {
	tests := []struct {
		name  string
		prURL *string
		want  string
	}{
		{"nil URL", nil, ""},
		{"valid URL", strPtr("https://github.com/owner/repo/pull/42"), "42"},
		{"URL with trailing number", strPtr("https://github.com/owner/repo/pull/123"), "123"},
		{"no slash in URL", strPtr("nourl"), ""},
		{"single segment URL", strPtr("42"), ""},
		{"trailing slash stripped", strPtr("https://github.com/owner/repo/pull/99"), "99"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &run.State{PRURL: tt.prURL}
			got := extractPRNumber(s)
			if got != tt.want {
				t.Errorf("extractPRNumber(%v) = %q, want %q", tt.prURL, got, tt.want)
			}
		})
	}
}

func TestFormatPR(t *testing.T) {
	tests := []struct {
		name  string
		prURL *string
		want  string
	}{
		{"nil URL", nil, "-"},
		{"typical URL", strPtr("https://github.com/owner/repo/pull/22"), "#22"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &run.State{PRURL: tt.prURL}
			got := formatPR(s)
			if got != tt.want {
				t.Errorf("formatPR() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetermineStatus(t *testing.T) {
	tests := []struct {
		name string
		s    *run.State
		want string
	}{
		{
			name: "no PR URL returns exited",
			s:    &run.State{},
			want: "exited",
		},
		{
			name: "with PR URL returns pr-created",
			s:    &run.State{PRURL: strPtr("https://github.com/owner/repo/pull/1")},
			want: "pr-created",
		},
		{
			name: "session type with missing worktree returns ended",
			s:    &run.State{Type: "session", Worktree: "/nonexistent/path/that/does/not/exist"},
			want: "ended",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineStatus(tt.s)
			if got != tt.want {
				t.Errorf("determineStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is a longer string", 10, "this is a ..."},
		{"this is a long string", 10, "this is a ..."},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := truncate(tt.input, tt.max)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
		})
	}
}

func TestFormatCost(t *testing.T) {
	cost := 1.5
	budget := "5"
	tests := []struct {
		name string
		s    *run.State
		want string
	}{
		{"no cost or budget", &run.State{}, "-"},
		{"with cost", &run.State{CostUSD: &cost}, "$1.50"},
		{"with budget only", &run.State{Budget: &budget}, "<$5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatCost(tt.s)
			if got != tt.want {
				t.Errorf("formatCost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGHCommandArgOrder(t *testing.T) {
	// Verify that all gh command builders place flags BEFORE the "--" separator
	// and the PR ref AFTER it. This prevents the bug where flags after "--"
	// are treated as positional arguments by gh.
	tests := []struct {
		name string
		fn   func(string) []string
		want []string
	}{
		{
			name: "getPRState args with number",
			fn:   ghPRStateArgs,
			want: []string{"pr", "view", "--json", "state", "-q", ".state", "--", "42"},
		},
		{
			name: "getPRCI args with number",
			fn:   ghPRChecksArgs,
			want: []string{"pr", "checks", "--", "42"},
		},
		{
			name: "getPRConflicts args with number",
			fn:   ghPRConflictsArgs,
			want: []string{"pr", "view", "--json", "mergeable", "-q", ".mergeable", "--", "42"},
		},
		{
			name: "getPRReviewDecision args with number",
			fn:   ghPRReviewDecisionArgs,
			want: []string{"pr", "view", "--json", "reviewDecision", "-q", ".reviewDecision", "--", "42"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.fn("42")
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("args = %v, want %v", got, tt.want)
			}

			// Verify structural invariant: all flags appear before "--"
			separatorIdx := -1
			for i, arg := range got {
				if arg == "--" {
					separatorIdx = i
					break
				}
			}
			if separatorIdx == -1 {
				t.Fatal("missing '--' separator")
			}
			for i := separatorIdx + 1; i < len(got); i++ {
				if got[i][0] == '-' {
					t.Errorf("flag %q found after '--' separator at index %d", got[i], i)
				}
			}
		})
	}
}

func TestGHCommandArgsAcceptURL(t *testing.T) {
	// Verify that gh command builders work with full PR URLs, which allows
	// gh to resolve PRs regardless of the current working directory.
	url := "https://github.com/owner/repo/pull/42"
	fns := []struct {
		name string
		fn   func(string) []string
	}{
		{"ghPRStateArgs", ghPRStateArgs},
		{"ghPRChecksArgs", ghPRChecksArgs},
		{"ghPRConflictsArgs", ghPRConflictsArgs},
		{"ghPRReviewDecisionArgs", ghPRReviewDecisionArgs},
	}
	for _, tt := range fns {
		t.Run(tt.name, func(t *testing.T) {
			args := tt.fn(url)
			// The URL should be the last argument, after "--"
			last := args[len(args)-1]
			if last != url {
				t.Errorf("last arg = %q, want %q", last, url)
			}
		})
	}
}

func TestExtractPRRef(t *testing.T) {
	tests := []struct {
		name  string
		prURL *string
		want  string
	}{
		{"nil URL", nil, ""},
		{"valid URL", strPtr("https://github.com/owner/repo/pull/42"), "https://github.com/owner/repo/pull/42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &run.State{PRURL: tt.prURL}
			got := extractPRRef(s)
			if got != tt.want {
				t.Errorf("extractPRRef() = %q, want %q", got, tt.want)
			}
		})
	}
}

func strPtr(s string) *string {
	return &s
}

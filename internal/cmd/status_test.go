package cmd

import (
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

func strPtr(s string) *string {
	return &s
}

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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &run.State{PRURL: tt.prURL}
			got := extractPRNumber(s)
			if got != tt.want {
				t.Errorf("extractPRNumber() = %q, want %q", got, tt.want)
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

func strPtr(s string) *string { return &s }

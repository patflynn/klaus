package cmd

import "testing"

func TestFormatPaneTitle(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		issue  string
		prompt string
		want   string
	}{
		{
			name:   "with issue uses #issue prefix",
			id:     "20260306-1720-176a",
			issue:  "23",
			prompt: "fix the failing tests in pkg/auth",
			want:   "#23 fix the failing tests in pkg/auth",
		},
		{
			name:   "no issue uses short id prefix",
			id:     "20260306-1720-176a",
			issue:  "",
			prompt: "refactor the config loader",
			want:   "176a refactor the config loader",
		},
		{
			name:   "short prompt preserved fully",
			id:     "20260306-1720-176a",
			issue:  "5",
			prompt: "fix typo",
			want:   "#5 fix typo",
		},
		{
			name:   "short id kept as-is",
			id:     "abcd",
			issue:  "",
			prompt: "test",
			want:   "abcd test",
		},
		{
			name:   "very short id kept as-is",
			id:     "ab",
			issue:  "",
			prompt: "test",
			want:   "ab test",
		},
		{
			name:   "empty prompt with issue",
			id:     "20260306-1720-176a",
			issue:  "10",
			prompt: "",
			want:   "#10",
		},
		{
			name:   "whitespace prompt",
			id:     "20260306-1720-176a",
			issue:  "",
			prompt: "   ",
			want:   "176a",
		},
		{
			name:   "long prompt truncated at word boundary",
			id:     "20260306-1720-176a",
			issue:  "34",
			prompt: "Implement a klaus merge command for combining worktrees",
			want:   "#34 Implement a klaus merge command for",
		},
		{
			name:   "exactly 40 char prompt not truncated",
			id:     "20260306-1720-176a",
			issue:  "",
			prompt: "1234567890123456789012345678901234567890",
			want:   "176a 1234567890123456789012345678901234567890",
		},
		{
			name:   "41 char prompt truncated at word boundary",
			id:     "20260306-1720-176a",
			issue:  "",
			prompt: "fix the authentication flow in the server",
			want:   "176a fix the authentication flow in the",
		},
		{
			name:   "issue present ignores id",
			id:     "20260306-1720-176a",
			issue:  "99",
			prompt: "update README",
			want:   "#99 update README",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatPaneTitle(tt.id, tt.issue, tt.prompt)
			if got != tt.want {
				t.Errorf("FormatPaneTitle(%q, %q, %q) = %q, want %q",
					tt.id, tt.issue, tt.prompt, got, tt.want)
			}
		})
	}
}

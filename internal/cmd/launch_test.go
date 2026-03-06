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
			name:   "full context",
			id:     "20260306-1720-176a",
			issue:  "23",
			prompt: "fix the failing tests in pkg/auth",
			want:   "agent:176a #23 fix the failing test",
		},
		{
			name:   "no issue",
			id:     "20260306-1720-176a",
			issue:  "",
			prompt: "refactor the config loader",
			want:   "agent:176a refactor the config ",
		},
		{
			name:   "short prompt",
			id:     "20260306-1720-176a",
			issue:  "5",
			prompt: "fix typo",
			want:   "agent:176a #5 fix typo",
		},
		{
			name:   "short id",
			id:     "abcd",
			issue:  "1",
			prompt: "test",
			want:   "agent:abcd #1 test",
		},
		{
			name:   "very short id",
			id:     "ab",
			issue:  "",
			prompt: "test",
			want:   "agent:ab test",
		},
		{
			name:   "empty prompt",
			id:     "20260306-1720-176a",
			issue:  "10",
			prompt: "",
			want:   "agent:176a #10",
		},
		{
			name:   "whitespace prompt",
			id:     "20260306-1720-176a",
			issue:  "",
			prompt: "   ",
			want:   "agent:176a",
		},
		{
			name:   "exactly 20 char prompt",
			id:     "20260306-1720-176a",
			issue:  "",
			prompt: "12345678901234567890",
			want:   "agent:176a 12345678901234567890",
		},
		{
			name:   "21 char prompt truncated",
			id:     "20260306-1720-176a",
			issue:  "",
			prompt: "123456789012345678901",
			want:   "agent:176a 12345678901234567890",
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

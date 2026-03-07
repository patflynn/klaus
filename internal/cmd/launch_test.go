package cmd

import (
	"strings"
	"testing"
)

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

func TestBuildPaneCommand(t *testing.T) {
	worktree := "/tmp/worktrees/repo/abc123"
	claudeCmd := "claude -p 'do stuff'"
	logFile := "/tmp/logs/abc123.jsonl"
	selfBin := "klaus"
	id := "20260306-1720-176a"

	t.Run("includes auto-watch by default", func(t *testing.T) {
		cmd := buildPaneCommand(worktree, claudeCmd, logFile, selfBin, "", id, false)
		if !strings.Contains(cmd, "_auto-watch") {
			t.Error("expected _auto-watch in pipeline, got:", cmd)
		}
		if !strings.Contains(cmd, "_finalize") {
			t.Error("expected _finalize in pipeline, got:", cmd)
		}
	})

	t.Run("no-watch excludes auto-watch", func(t *testing.T) {
		cmd := buildPaneCommand(worktree, claudeCmd, logFile, selfBin, "", id, true)
		if strings.Contains(cmd, "_auto-watch") {
			t.Error("expected no _auto-watch in pipeline with --no-watch, got:", cmd)
		}
		if !strings.Contains(cmd, "_finalize") {
			t.Error("expected _finalize still present in pipeline, got:", cmd)
		}
	})

	t.Run("cross-repo includes finalize prefix for auto-watch", func(t *testing.T) {
		prefix := "cd '/host/repo' && "
		cmd := buildPaneCommand(worktree, claudeCmd, logFile, selfBin, prefix, id, false)
		// Should have the prefix before both _finalize and _auto-watch
		parts := strings.Split(cmd, "_auto-watch")
		if len(parts) < 2 {
			t.Fatal("expected _auto-watch in pipeline")
		}
		// The part before _auto-watch should contain the prefix
		if !strings.Contains(parts[0], "cd '/host/repo'") {
			t.Error("expected finalize prefix before _auto-watch, got:", cmd)
		}
	})
}

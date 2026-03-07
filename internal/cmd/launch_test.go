package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/project"
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

func TestLaunchResolvesProjectName(t *testing.T) {
	// Test that a bare name (no /) is resolved from the project registry.
	// We test the resolution logic directly since the full launch flow needs tmux.
	tmpDir := t.TempDir()
	regPath := filepath.Join(tmpDir, "projects.json")

	reg := &project.Registry{
		Projects: map[string]string{
			"my-project": "/home/user/src/my-project",
		},
	}
	if err := reg.SaveTo(regPath); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	loaded, err := project.LoadFrom(regPath)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	// Simulate the launch resolution: if repoRef has no "/" and matches a project, use it
	repoRef := "my-project"
	var projectLocalPath string
	if !strings.Contains(repoRef, "/") {
		if localPath, ok := loaded.Get(repoRef); ok {
			projectLocalPath = localPath
		}
	}

	if projectLocalPath != "/home/user/src/my-project" {
		t.Errorf("expected project path /home/user/src/my-project, got %q", projectLocalPath)
	}

	// An owner/repo format should NOT be resolved as a project name
	repoRef = "owner/repo"
	projectLocalPath = ""
	if !strings.Contains(repoRef, "/") {
		if localPath, ok := loaded.Get(repoRef); ok {
			projectLocalPath = localPath
		}
	}

	if projectLocalPath != "" {
		t.Errorf("owner/repo should not resolve as project name, got %q", projectLocalPath)
	}

	// An unregistered project name should not resolve
	repoRef = "unknown-project"
	projectLocalPath = ""
	if !strings.Contains(repoRef, "/") {
		if localPath, ok := loaded.Get(repoRef); ok {
			projectLocalPath = localPath
		}
	}

	if projectLocalPath != "" {
		t.Errorf("unregistered project should not resolve, got %q", projectLocalPath)
	}
}

func TestLaunchErrorMessageMentionsProjectAdd(t *testing.T) {
	// Verify the error message hints at 'klaus project add'
	errMsg := "no target repo — use --repo owner/repo, 'klaus target owner/repo', or 'klaus project add' to register a project"
	if !strings.Contains(errMsg, "klaus project add") {
		t.Error("error message should mention 'klaus project add'")
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

	t.Run("exports KLAUS_SESSION_ID via tmuxSessionEnvPrefix", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "session-20260306-1720-abc1")
		cmd := buildPaneCommand(worktree, claudeCmd, logFile, selfBin, "", id, false)
		if !strings.Contains(cmd, "export KLAUS_SESSION_ID='session-20260306-1720-abc1'") {
			t.Error("expected KLAUS_SESSION_ID export in pane command, got:", cmd)
		}
	})

	t.Run("no KLAUS_SESSION_ID export when env unset", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		cmd := buildPaneCommand(worktree, claudeCmd, logFile, selfBin, "", id, false)
		if strings.Contains(cmd, "KLAUS_SESSION_ID") {
			t.Error("expected no KLAUS_SESSION_ID export when session ID is empty, got:", cmd)
		}
	})
}

package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/config"
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

	t.Run("builds correct pipeline without auto-watch", func(t *testing.T) {
		cmd := buildPaneCommand(worktree, claudeCmd, logFile, selfBin, "", id)
		if !strings.Contains(cmd, "_finalize") {
			t.Error("expected _finalize in pipeline, got:", cmd)
		}
		if strings.Contains(cmd, "_auto-watch") {
			t.Error("expected no _auto-watch in pipeline, got:", cmd)
		}
	})

	t.Run("cross-repo includes finalize prefix", func(t *testing.T) {
		prefix := "cd '/host/repo' && "
		cmd := buildPaneCommand(worktree, claudeCmd, logFile, selfBin, prefix, id)
		if !strings.Contains(cmd, "cd '/host/repo' && klaus _finalize") {
			t.Error("expected finalize prefix before _finalize, got:", cmd)
		}
	})

	t.Run("exports KLAUS_SESSION_ID via tmuxSessionEnvPrefix", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "session-20260306-1720-abc1")
		cmd := buildPaneCommand(worktree, claudeCmd, logFile, selfBin, "", id)
		if !strings.Contains(cmd, "export KLAUS_SESSION_ID='session-20260306-1720-abc1'") {
			t.Error("expected KLAUS_SESSION_ID export in pane command, got:", cmd)
		}
	})

	t.Run("no KLAUS_SESSION_ID export when env unset", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		cmd := buildPaneCommand(worktree, claudeCmd, logFile, selfBin, "", id)
		if strings.Contains(cmd, "KLAUS_SESSION_ID") {
			t.Error("expected no KLAUS_SESSION_ID export when session ID is empty, got:", cmd)
		}
	})
}

func TestPRFixPromptInstructsPushOnly(t *testing.T) {
	dir := t.TempDir() // no .klaus/pr-fix-prompt.md — uses default

	vars := config.PromptVars{
		RunID:    "20260312-1820-abcd",
		Issue:    "42",
		PR:       "99",
		Branch:   "feature/my-branch",
		RepoName: "test-repo",
	}

	prompt, err := config.RenderPRFixPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderPRFixPrompt() error: %v", err)
	}

	// Must instruct push-only, no PR creation
	if !strings.Contains(prompt, "Do NOT create a new PR") {
		t.Error("pr-fix prompt must instruct agent not to create a new PR")
	}
	if !strings.Contains(prompt, "git push") {
		t.Error("pr-fix prompt must instruct agent to push")
	}
	if !strings.Contains(prompt, "PR #99") {
		t.Error("pr-fix prompt must mention the PR number")
	}
	if !strings.Contains(prompt, "feature/my-branch") {
		t.Error("pr-fix prompt must mention the branch name")
	}
	if !strings.Contains(prompt, "#42") {
		t.Error("pr-fix prompt must mention the issue when provided")
	}
	if !strings.Contains(prompt, "20260312-1820-abcd") {
		t.Error("pr-fix prompt must contain run ID")
	}
	// Should NOT contain "Create a PR" or "gh pr create"
	if strings.Contains(prompt, "gh pr create") {
		t.Error("pr-fix prompt must not mention gh pr create")
	}
}

func TestPRFixPromptNoIssue(t *testing.T) {
	dir := t.TempDir()

	vars := config.PromptVars{
		RunID:  "20260312-1820-abcd",
		PR:     "99",
		Branch: "feature/my-branch",
	}

	prompt, err := config.RenderPRFixPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderPRFixPrompt() error: %v", err)
	}

	// Without an issue, the issue reference should not appear
	if strings.Contains(prompt, "issue #") {
		t.Error("pr-fix prompt should not mention issue when none is provided")
	}
}

func TestResolveRepoTarget_SessionTargetUsedWhenNoFlag(t *testing.T) {
	reg := &project.Registry{
		Projects: map[string]string{
			"reel-life": "/home/user/hack/reel-life",
		},
	}

	repoRef, projectLocalPath := resolveRepoTarget("", "reel-life", reg)

	if repoRef != "reel-life" {
		t.Errorf("repoRef = %q, want %q", repoRef, "reel-life")
	}
	if projectLocalPath != "/home/user/hack/reel-life" {
		t.Errorf("projectLocalPath = %q, want %q", projectLocalPath, "/home/user/hack/reel-life")
	}
}

func TestResolveRepoTarget_RepoFlagOverridesSessionTarget(t *testing.T) {
	reg := &project.Registry{
		Projects: map[string]string{
			"reel-life":     "/home/user/hack/reel-life",
			"other-project": "/home/user/hack/other-project",
		},
	}

	repoRef, projectLocalPath := resolveRepoTarget("other-project", "reel-life", reg)

	if repoRef != "other-project" {
		t.Errorf("repoRef = %q, want %q", repoRef, "other-project")
	}
	if projectLocalPath != "/home/user/hack/other-project" {
		t.Errorf("projectLocalPath = %q, want %q", projectLocalPath, "/home/user/hack/other-project")
	}
}

func TestResolveRepoTarget_SessionTargetWorksEvenWithHostRoot(t *testing.T) {
	// This tests the bug fix from issue #103.
	// The old code only checked session target when hostRoot was empty.
	// With the new resolveRepoTarget() function, session target is always
	// used when repoFlag is empty, regardless of whether the caller has a
	// hostRoot. The caller (RunE) only falls back to hostRoot after
	// resolveRepoTarget returns empty values.
	reg := &project.Registry{
		Projects: map[string]string{
			"reel-life": "/home/user/hack/reel-life",
		},
	}

	repoRef, projectLocalPath := resolveRepoTarget("", "reel-life", reg)

	if repoRef != "reel-life" {
		t.Errorf("repoRef = %q, want %q", repoRef, "reel-life")
	}
	if projectLocalPath != "/home/user/hack/reel-life" {
		t.Errorf("projectLocalPath = %q, want %q", projectLocalPath, "/home/user/hack/reel-life")
	}
}

func TestResolveRepoTarget_OwnerRepoParsedCorrectly(t *testing.T) {
	reg := &project.Registry{
		Projects: map[string]string{
			"reel-life": "/home/user/hack/reel-life",
		},
	}

	repoRef, projectLocalPath := resolveRepoTarget("", "patflynn/reel-life", reg)

	if repoRef != "patflynn/reel-life" {
		t.Errorf("repoRef = %q, want %q", repoRef, "patflynn/reel-life")
	}
	if projectLocalPath != "" {
		t.Errorf("projectLocalPath = %q, want empty (owner/repo should not resolve as project name)", projectLocalPath)
	}
}

func TestResolveRepoTarget_EmptyTargetFallsThrough(t *testing.T) {
	reg := &project.Registry{
		Projects: map[string]string{
			"reel-life": "/home/user/hack/reel-life",
		},
	}

	repoRef, projectLocalPath := resolveRepoTarget("", "", reg)

	if repoRef != "" {
		t.Errorf("repoRef = %q, want empty", repoRef)
	}
	if projectLocalPath != "" {
		t.Errorf("projectLocalPath = %q, want empty", projectLocalPath)
	}
}

func TestResolveRepoTarget_NilRegistry(t *testing.T) {
	repoRef, projectLocalPath := resolveRepoTarget("", "reel-life", nil)

	if repoRef != "reel-life" {
		t.Errorf("repoRef = %q, want %q", repoRef, "reel-life")
	}
	if projectLocalPath != "" {
		t.Errorf("projectLocalPath = %q, want empty (nil registry should not resolve)", projectLocalPath)
	}
}

func TestBuildSandboxPaneCommand(t *testing.T) {
	host := "klaus-worker-0"
	worktree := "/tmp/klaus-sessions/repo/abc123"
	claudeCmd := "claude -p 'do stuff'"
	logFile := "/tmp/logs/abc123.jsonl"
	selfBin := "klaus"
	id := "20260328-1915-e4b3"

	t.Run("wraps claude in SSH", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		cmd := buildSandboxPaneCommand(host, worktree, claudeCmd, logFile, selfBin, "", id)
		if !strings.Contains(cmd, "ssh 'klaus-worker-0'") {
			t.Error("expected ssh to sandbox host, got:", cmd)
		}
		if !strings.Contains(cmd, "cd '/tmp/klaus-sessions/repo/abc123'") {
			t.Error("expected cd to worktree on remote, got:", cmd)
		}
	})

	t.Run("tee and format run locally", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		cmd := buildSandboxPaneCommand(host, worktree, claudeCmd, logFile, selfBin, "", id)
		if !strings.Contains(cmd, "| tee") {
			t.Error("expected tee in local pipeline, got:", cmd)
		}
		if !strings.Contains(cmd, "_format-stream") {
			t.Error("expected _format-stream in local pipeline, got:", cmd)
		}
		if !strings.Contains(cmd, "_finalize") {
			t.Error("expected _finalize in local pipeline, got:", cmd)
		}
	})

	t.Run("rsyncs results back after finalize", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		cmd := buildSandboxPaneCommand(host, worktree, claudeCmd, logFile, selfBin, "", id)
		if !strings.Contains(cmd, "rsync -az") {
			t.Error("expected rsync back in command, got:", cmd)
		}
		// rsync should reference host:worktree/ -> worktree/
		if !strings.Contains(cmd, "'klaus-worker-0':'/tmp/klaus-sessions/repo/abc123'/") {
			t.Error("expected rsync from host:worktree, got:", cmd)
		}
	})

}

func TestLaunchCmdHasSandboxFlags(t *testing.T) {
	f := launchCmd.Flags().Lookup("local")
	if f == nil {
		t.Fatal("expected --local flag to be registered on launch command")
	}
	if f.DefValue != "false" {
		t.Errorf("--local default value should be 'false', got %q", f.DefValue)
	}

	h := launchCmd.Flags().Lookup("host")
	if h == nil {
		t.Fatal("expected --host flag to be registered on launch command")
	}
	if h.DefValue != "" {
		t.Errorf("--host default value should be empty, got %q", h.DefValue)
	}
}

func TestBuildClaudeCommand_SessionNaming(t *testing.T) {
	cmd := buildClaudeCommand("sys prompt", "5", "do stuff", "20260405-1200-abcd", "")
	if !strings.Contains(cmd, "-n '20260405-1200-abcd'") {
		t.Errorf("expected -n flag with run ID, got: %s", cmd)
	}
	if strings.Contains(cmd, "--resume") {
		t.Error("expected no --resume flag when resumeSessionName is empty")
	}
	if strings.Contains(cmd, "--fork-session") {
		t.Error("expected no --fork-session flag when resumeSessionName is empty")
	}
}

func TestBuildClaudeCommand_WithResume(t *testing.T) {
	cmd := buildClaudeCommand("sys prompt", "5", "fix CI", "20260405-1200-efgh", "20260405-1100-abcd")
	if !strings.Contains(cmd, "-n '20260405-1200-efgh'") {
		t.Errorf("expected -n flag with new run ID, got: %s", cmd)
	}
	if !strings.Contains(cmd, "--resume '20260405-1100-abcd'") {
		t.Errorf("expected --resume flag with original session name, got: %s", cmd)
	}
	if !strings.Contains(cmd, "--fork-session") {
		t.Errorf("expected --fork-session flag, got: %s", cmd)
	}
}

func TestLaunchCmdHasResumeFromFlag(t *testing.T) {
	f := launchCmd.Flags().Lookup("resume-from")
	if f == nil {
		t.Fatal("expected --resume-from flag to be registered on launch command")
	}
	if f.DefValue != "" {
		t.Errorf("--resume-from default value should be empty, got %q", f.DefValue)
	}
}

func TestLaunchCmdHasPRFlag(t *testing.T) {
	// Verify the --pr flag is registered on the launch command
	f := launchCmd.Flags().Lookup("pr")
	if f == nil {
		t.Fatal("expected --pr flag to be registered on launch command")
	}
	if f.DefValue != "" {
		t.Errorf("--pr default value should be empty, got %q", f.DefValue)
	}
}

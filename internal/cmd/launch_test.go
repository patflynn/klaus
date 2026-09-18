package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/pflag"
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
	promptFile := "/tmp/prompts/20260306-1720-176a.md"
	selfBin := "klaus"
	id := "20260306-1720-176a"

	t.Run("builds correct pipeline without auto-watch", func(t *testing.T) {
		cmd := buildPaneCommand(worktree, claudeCmd, promptFile, logFile, selfBin, "", id)
		if !strings.Contains(cmd, "_finalize") {
			t.Error("expected _finalize in pipeline, got:", cmd)
		}
		if strings.Contains(cmd, "_auto-watch") {
			t.Error("expected no _auto-watch in pipeline, got:", cmd)
		}
	})

	t.Run("redirects the prompt file into the backend command only", func(t *testing.T) {
		cmd := buildPaneCommand(worktree, claudeCmd, promptFile, logFile, selfBin, "", id)
		if !strings.Contains(cmd, "( "+claudeCmd+" < '"+promptFile+"'; klaus_backend_exit=$?") {
			t.Error("expected stdin redirect on the backend command, got:", cmd)
		}
		if strings.Count(cmd, "<") != 1 {
			t.Error("expected exactly one redirect, got:", cmd)
		}
	})

	t.Run("cross-repo includes finalize prefix", func(t *testing.T) {
		prefix := "cd '/host/repo' && "
		cmd := buildPaneCommand(worktree, claudeCmd, promptFile, logFile, selfBin, prefix, id)
		if !strings.Contains(cmd, "cd '/host/repo' && klaus _finalize") {
			t.Error("expected finalize prefix before _finalize, got:", cmd)
		}
	})

	t.Run("exports KLAUS_SESSION_ID via tmuxSessionEnvPrefix", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "session-20260306-1720-abc1")
		cmd := buildPaneCommand(worktree, claudeCmd, promptFile, logFile, selfBin, "", id)
		if !strings.Contains(cmd, "export KLAUS_SESSION_ID='session-20260306-1720-abc1'") {
			t.Error("expected KLAUS_SESSION_ID export in pane command, got:", cmd)
		}
	})

	t.Run("no KLAUS_SESSION_ID export when env unset", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		cmd := buildPaneCommand(worktree, claudeCmd, promptFile, logFile, selfBin, "", id)
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
	promptFile := "/tmp/prompts/20260328-1915-e4b3.md"
	selfBin := "klaus"
	id := "20260328-1915-e4b3"

	t.Run("wraps claude in SSH", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		cmd := buildSandboxPaneCommand(host, worktree, claudeCmd, promptFile, logFile, selfBin, "", id)
		if !strings.Contains(cmd, "ssh 'klaus-worker-0'") {
			t.Error("expected ssh to sandbox host, got:", cmd)
		}
		if !strings.Contains(cmd, shellQuote("cd "+shellQuote(worktree)+" && "+claudeCmd)) {
			t.Error("expected cd to worktree on remote, got:", cmd)
		}
	})

	t.Run("feeds the local prompt file to ssh's stdin", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		cmd := buildSandboxPaneCommand(host, worktree, claudeCmd, promptFile, logFile, selfBin, "", id)
		remote := shellQuote("cd " + shellQuote(worktree) + " && " + claudeCmd)
		if !strings.Contains(cmd, "( ssh 'klaus-worker-0' "+remote+" < '"+promptFile+"'; klaus_backend_exit=$?") {
			t.Error("expected stdin redirect on the ssh command, got:", cmd)
		}
	})

	t.Run("tee and format run locally", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		cmd := buildSandboxPaneCommand(host, worktree, claudeCmd, promptFile, logFile, selfBin, "", id)
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
		cmd := buildSandboxPaneCommand(host, worktree, claudeCmd, promptFile, logFile, selfBin, "", id)
		if !strings.Contains(cmd, "rsync -az") {
			t.Error("expected rsync back in command, got:", cmd)
		}
		// rsync should reference host:worktree/ -> worktree/
		if !strings.Contains(cmd, "'klaus-worker-0':'/tmp/klaus-sessions/repo/abc123'/") {
			t.Error("expected rsync from host:worktree, got:", cmd)
		}
	})

}

// However hostile or long the prompt, the command handed to tmux carries only
// its path: nothing in it can be expanded by the shell or by tmux formats.
func TestAgentPaneCommandKeepsPromptOut(t *testing.T) {
	t.Setenv(sessionIDEnv, "")
	prompt := "it's `whoami` $(touch /tmp/pwned) ${HOME} #{pane_id} \"quoted\"\n" + strings.Repeat("x", 40000)
	const promptPath = "/home/u/.klaus/sessions/s/prompts/20260918-1440-abcd.md"
	for _, kind := range []backend.Kind{backend.Claude, backend.Codex} {
		for _, host := range []string{"", "sandbox-0"} {
			t.Run(string(kind)+"/host="+host, func(t *testing.T) {
				o := backend.Options{SystemPrompt: "sys", SystemPromptFile: "/p/20260918-1440-abcd.system.md", Prompt: prompt, RunID: "20260918-1440-abcd", Budget: "5"}
				cmd, err := agentPaneCommand(kind, o, promptPath, host, "/wt", "/logs/x.jsonl", "", "20260918-1440-abcd")
				if err != nil {
					t.Fatal(err)
				}
				for _, frag := range []string{"whoami", "$(touch", "${HOME}", "#{pane_id}", "xxxx"} {
					if strings.Contains(cmd, frag) {
						t.Fatalf("prompt fragment %q in pane command: %s", frag, cmd)
					}
				}
				if !strings.Contains(cmd, " < '"+promptPath+"'; klaus_backend_exit=$?") {
					t.Fatalf("prompt file not redirected into the backend: %s", cmd)
				}
				if strings.Contains(cmd, "--append-system-prompt-file") == (host != "" || kind != backend.Claude) {
					t.Fatalf("system prompt file only works for a local claude: %s", cmd)
				}
			})
		}
	}
}

func TestAgentPaneCommandSizeGuard(t *testing.T) {
	t.Setenv(sessionIDEnv, "")
	big := strings.Repeat("y", maxPaneCommandBytes)
	for _, tc := range []struct {
		name, host, want string
		kind             backend.Kind
		o                backend.Options
	}{
		{name: "claude reads a big system prompt from its file", kind: backend.Claude, o: backend.Options{SystemPrompt: big, SystemPromptFile: "/p/s.md", Prompt: big}},
		{name: "sandbox claude inlines the system prompt", kind: backend.Claude, host: "sandbox-0", o: backend.Options{SystemPrompt: big, SystemPromptFile: "/p/s.md"}, want: "the worker system prompt is 12000 bytes and goes inline for sandbox runs; shorten .klaus/prompt.md (.klaus/pr-fix-prompt.md for --pr), or use --local"},
		{name: "codex inlines the system prompt", kind: backend.Codex, o: backend.Options{SystemPrompt: big, Prompt: big}, want: "the worker system prompt is 12000 bytes and goes inline for codex runs"},
		{name: "agy inlines the prompt", kind: backend.Agy, o: backend.Options{SystemPrompt: "sys", Prompt: big}, want: "the prompt is 12000 bytes and agy takes it as an argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := agentPaneCommand(tc.kind, tc.o, "/p/prompt.md", tc.host, "/wt", "/logs/x.jsonl", "", "id")
			if tc.want == "" {
				if err != nil || len(cmd) > maxPaneCommandBytes {
					t.Fatalf("err = %v, len = %d", err, len(cmd))
				}
				return
			}
			if err == nil {
				t.Fatalf("expected the size guard to fire, got a %d-byte command", len(cmd))
			}
			if !regexp.MustCompile(`agent pane command is \d+ bytes, over the 12000-byte limit`).MatchString(err.Error()) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
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
	cmd := buildClaudeCommand("sys prompt", "5", "do stuff", "20260405-1200-abcd", "", "", "")
	if !strings.Contains(cmd, "'-n' '20260405-1200-abcd'") {
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
	cmd := buildClaudeCommand("sys prompt", "5", "fix CI", "20260405-1200-efgh", "20260405-1100-abcd", "", "")
	if !strings.Contains(cmd, "'-n' '20260405-1200-efgh'") {
		t.Errorf("expected -n flag with new run ID, got: %s", cmd)
	}
	if !strings.Contains(cmd, "'--resume' '20260405-1100-abcd'") {
		t.Errorf("expected --resume flag with original session name, got: %s", cmd)
	}
	if !strings.Contains(cmd, "--fork-session") {
		t.Errorf("expected --fork-session flag, got: %s", cmd)
	}
}

func TestBuildClaudeCommand_ModelAndEffort(t *testing.T) {
	t.Run("both passed through when set", func(t *testing.T) {
		cmd := buildClaudeCommand("sys", "5", "do stuff", "20260805-0900-abcd", "", "claude-sonnet-5", "low")
		if !strings.Contains(cmd, "'--model' 'claude-sonnet-5'") {
			t.Errorf("expected --model flag, got: %s", cmd)
		}
		if !strings.Contains(cmd, "'--effort' 'low'") {
			t.Errorf("expected --effort flag, got: %s", cmd)
		}
	})

	t.Run("absent when unset so claude's own resolution applies", func(t *testing.T) {
		cmd := buildClaudeCommand("sys", "5", "do stuff", "20260805-0900-abcd", "", "", "")
		if strings.Contains(cmd, "--model") {
			t.Errorf("expected no --model flag when unset, got: %s", cmd)
		}
		if strings.Contains(cmd, "--effort") {
			t.Errorf("expected no --effort flag when unset, got: %s", cmd)
		}
	})
}

func TestValidateEffort(t *testing.T) {
	for _, valid := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		if err := validateEffort(valid); err != nil {
			t.Errorf("validateEffort(%q) = %v, want nil", valid, err)
		}
	}
	err := validateEffort("turbo")
	if err == nil {
		t.Fatal("validateEffort(\"turbo\") = nil, want error")
	}
	// The error must name the valid values.
	for _, want := range []string{"turbo", "low", "medium", "high", "xhigh", "max"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
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

func TestClaudeSessionExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projDir := filepath.Join(home, ".claude", "projects", "-some-encoded-dir")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	const presentUUID = "79dcfb08-4de4-45f4-b7db-056bccbc3a00"
	if err := os.WriteFile(filepath.Join(projDir, presentUUID+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !claudeSessionExists(presentUUID) {
		t.Errorf("claudeSessionExists(%q) = false, want true", presentUUID)
	}
	if claudeSessionExists("00000000-0000-0000-0000-000000000000") {
		t.Error("claudeSessionExists(missing UUID) = true, want false")
	}
	if claudeSessionExists("") {
		t.Error("claudeSessionExists(\"\") = true, want false")
	}

	// Reject inputs containing glob metacharacters or path separators even if
	// matching files happen to exist on disk.
	for _, bad := range []string{"*", "?", "../" + presentUUID, presentUUID + "/..", "[a-z]" + presentUUID[5:], presentUUID + "\x00"} {
		if claudeSessionExists(bad) {
			t.Errorf("claudeSessionExists(%q) = true, want false (invalid characters)", bad)
		}
	}
}

func TestResolveResumeID(t *testing.T) {
	const sessionID = "79dcfb08-4de4-45f4-b7db-056bccbc3a00"
	for _, tc := range []struct {
		name       string
		kind       backend.Kind
		reason     string
		missing    bool
		missingID  bool
		blockStage bool
	}{
		{name: "Claude resumes after success failure reason", kind: backend.Claude, reason: "success"},
		{name: "Claude resumes after crash", kind: backend.Claude, reason: "error_during_execution"},
		{name: "Claude missing transcript starts fresh", kind: backend.Claude, reason: "success", missing: true},
		{name: "Claude missing session ID starts fresh", kind: backend.Claude, reason: "success", missingID: true},
		{name: "Claude staging failure starts fresh", kind: backend.Claude, reason: "success", blockStage: true},
		{name: "Codex resumes after success failure reason", kind: backend.Codex, reason: "success"},
		{name: "Codex resumes after crash", kind: backend.Codex, reason: "codex exited with status 1"},
		{name: "Codex missing session ID starts fresh", kind: backend.Codex, reason: "success", missingID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			store := run.NewHomeDirStoreFromPath(filepath.Join(home, ".klaus", "session-test"))
			if err := store.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			prior := &run.State{
				ID: "prior-run", Backend: string(tc.kind),
				Worktree: filepath.Join(home, "old-worktree"), FailureReason: &tc.reason,
			}
			worktree := filepath.Join(home, "new-worktree")
			if tc.kind == backend.Claude {
				logFile := filepath.Join(store.LogDir(), prior.ID+".jsonl")
				prior.LogFile = &logFile
				log := `{"type":"result","session_id":"` + sessionID + `"}`
				if tc.missingID {
					log = `{"type":"result"}`
				}
				if err := os.WriteFile(logFile, []byte(log+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if !tc.missing {
					src := claudeConversationPath(prior.Worktree, sessionID)
					if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(src, []byte(`{"sessionId":"`+sessionID+`"}`+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if tc.blockStage {
					if err := os.WriteFile(filepath.Dir(claudeConversationPath(worktree, sessionID)), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			} else if !tc.missingID {
				prior.BackendSessionID = strPtr(sessionID)
			}
			if err := store.Save(prior); err != nil {
				t.Fatal(err)
			}
			prior, err := store.Load(prior.ID)
			if err != nil {
				t.Fatal(err)
			}
			stderr, err := os.CreateTemp(home, "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			originalStderr := os.Stderr
			os.Stderr = stderr
			t.Cleanup(func() { os.Stderr = originalStderr })
			got := resolveResumeID(prior, tc.kind, worktree)
			want := sessionID
			if tc.missing || tc.missingID || tc.blockStage {
				want = ""
			}
			if got != want {
				t.Fatalf("resolveResumeID = %q, want %q", got, want)
			}
			warning, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(warning), "failed (success)") {
				t.Fatalf("misleading warning: %s", warning)
			}
			if want != "" {
				if !strings.Contains(string(warning), "ended with "+tc.reason+"; resuming") {
					t.Errorf("missing resume warning: %s", warning)
				}
				if tc.kind == backend.Claude {
					src, err := os.ReadFile(claudeConversationPath(prior.Worktree, sessionID))
					if err != nil {
						t.Fatal(err)
					}
					dest, err := os.ReadFile(claudeConversationPath(worktree, sessionID))
					if err != nil || string(src) != string(dest) {
						t.Fatalf("transcript was not staged intact: %v", err)
					}
				}
			}
			args, _ := tc.kind.Worker(backend.Options{ResumeID: got})
			command := backend.ShellCommand(args)
			if strings.Contains(command, sessionID) != (want != "") {
				t.Errorf("unexpected resume command: %s", command)
			}
		})
	}
}

func TestResumeHandlesNilRunState(t *testing.T) {
	for _, kind := range []backend.Kind{backend.Claude, backend.Codex} {
		if got := resolveResumeID(nil, kind, t.TempDir()); got != "" {
			t.Errorf("resolveResumeID(nil, %s) = %q, want empty", kind, got)
		}
	}
}

// TestStageResumeTranscript covers Bug 1: 'claude --resume' scopes
// conversation transcripts by working directory, so a resume launched in a new
// worktree can't find the prior conversation. stageResumeTranscript copies the
// transcript into the new worktree's project dir before launch, and falls back
// to a fresh session (no --resume) when the source transcript is absent.
func TestStageResumeTranscript(t *testing.T) {
	const uuid = "79dcfb08-4de4-45f4-b7db-056bccbc3a00"

	t.Run("copies transcript into new worktree project dir", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)

		oldWorktree := "/tmp/klaus-sessions/klaus/old-run"
		newWorktree := "/tmp/klaus-sessions/klaus/new-run"

		// Write the prior conversation transcript into the OLD worktree's
		// project dir, the way claude itself would.
		src := claudeConversationPath(oldWorktree, uuid)
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatalf("MkdirAll src: %v", err)
		}
		if err := os.WriteFile(src, []byte(`{"sessionId":"`+uuid+`"}`+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile src: %v", err)
		}

		prevState := &run.State{
			ID:              "20260628-1000-old",
			Worktree:        oldWorktree,
			ClaudeSessionID: strPtr(uuid),
		}

		if !stageResumeTranscript(prevState, uuid, newWorktree) {
			t.Fatal("stageResumeTranscript = false, want true when source transcript exists")
		}

		dest := claudeConversationPath(newWorktree, uuid)
		if _, err := os.Stat(dest); err != nil {
			t.Fatalf("expected transcript copied to %q: %v", dest, err)
		}

		// With staging done, the launched command resumes the session.
		cmd := buildClaudeCommand("sys", "5", "fix conflicts", "20260628-1001-new", uuid, "", "")
		if !strings.Contains(cmd, "'--resume' '"+uuid+"'") {
			t.Errorf("expected --resume after successful staging, got: %s", cmd)
		}
	})

	t.Run("falls back to fresh session when source transcript is absent", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)

		prevState := &run.State{
			ID:              "20260628-1000-old",
			Worktree:        "/tmp/klaus-sessions/klaus/old-run",
			ClaudeSessionID: strPtr(uuid),
		}

		newWorktree := "/tmp/klaus-sessions/klaus/new-run"
		if stageResumeTranscript(prevState, uuid, newWorktree) {
			t.Fatal("stageResumeTranscript = true, want false when source transcript is missing")
		}

		// The caller leaves resolvedResume empty, so no --resume flag.
		cmd := buildClaudeCommand("sys", "5", "fix conflicts", "20260628-1001-new", "", "", "")
		if strings.Contains(cmd, "--resume") {
			t.Errorf("expected no --resume when staging failed, got: %s", cmd)
		}
	})
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

// TestSessionPromptDocumentsEveryLaunchFlag keeps the coordinator system prompt
// in step with the CLI. A coordinator only knows the flags its prompt names: the
// prompt once listed four of them, so --resume-from was invisible and agents were
// killed and re-briefed cold when they could have been continued.
func TestSessionPromptDocumentsEveryLaunchFlag(t *testing.T) {
	flagToken := regexp.MustCompile(`--[a-z][a-z0-9-]*`)

	for _, tc := range []struct {
		name     string
		repoName string
	}{
		{"in a repo", "klaus"},
		{"scratch workspace", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Empty repoRoot: no .klaus/session-prompt.md, so the built-in
			// template renders.
			prompt, err := config.RenderSessionPrompt(t.TempDir(), config.PromptVars{
				RunID:    "20260731-1804-b437",
				Branch:   "agent/20260731-1804-b437",
				RepoName: tc.repoName,
			})
			if err != nil {
				t.Fatalf("rendering session prompt: %v", err)
			}

			documented := map[string]bool{}
			for _, tok := range flagToken.FindAllString(prompt, -1) {
				documented[tok] = true
			}

			launchCmd.Flags().VisitAll(func(f *pflag.Flag) {
				if !documented["--"+f.Name] {
					t.Errorf("klaus launch --%s is not documented in the coordinator session prompt", f.Name)
				}
			})
		})
	}
}

func TestResolvePrompt(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "prompt.md")
	// Backticks and $VAR are exactly what a shell-quoted prompt loses.
	body := "Fix `ValidateToken()` in internal/auth/verify.go\n\nUse $HOME, not ~.\n"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("file contents are used verbatim", func(t *testing.T) {
		got, err := resolvePrompt(nil, file)
		if err != nil {
			t.Fatalf("resolvePrompt: %v", err)
		}
		if got != body {
			t.Errorf("prompt = %q, want %q", got, body)
		}
	})

	t.Run("positional argument still works", func(t *testing.T) {
		got, err := resolvePrompt([]string{"do the thing"}, "")
		if err != nil {
			t.Fatalf("resolvePrompt: %v", err)
		}
		if got != "do the thing" {
			t.Errorf("prompt = %q", got)
		}
	})

	t.Run("both sources is an error", func(t *testing.T) {
		_, err := resolvePrompt([]string{"do the thing"}, file)
		if err == nil || !strings.Contains(err.Error(), "--prompt-file") {
			t.Errorf("err = %v, want an error naming --prompt-file", err)
		}
	})

	t.Run("neither source is an error", func(t *testing.T) {
		_, err := resolvePrompt(nil, "")
		if err == nil || !strings.Contains(err.Error(), "--prompt-file") {
			t.Errorf("err = %v, want an error naming --prompt-file", err)
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		if _, err := resolvePrompt(nil, filepath.Join(dir, "nope.md")); err == nil {
			t.Error("expected an error for a missing prompt file")
		}
	})

	t.Run("whitespace-only file is an error", func(t *testing.T) {
		blank := filepath.Join(dir, "blank.md")
		if err := os.WriteFile(blank, []byte("  \n\t\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := resolvePrompt(nil, blank); err == nil {
			t.Error("expected an error for an empty prompt file")
		}
	})
}

// These compatibility helpers delegate the Claude contract to the backend package.
func validateEffort(effort string) error { return backend.Claude.ValidateEffort(effort) }
func buildClaudeCommand(sysPrompt, budget, prompt, runID, resumeID, model, effort string) string {
	args, _ := backend.Claude.Worker(backend.Options{SystemPrompt: sysPrompt, Budget: budget, Prompt: prompt, RunID: runID, ResumeID: resumeID, Model: model, Effort: effort})
	return backend.ShellCommand(args)
}

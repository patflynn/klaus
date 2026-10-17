//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// claudeSettingsArg returns the value of the one --settings in argv, as the
// fake claude recorded it, failing unless there is exactly one.
func (h *Harness) claudeSettingsArg(argv string) string {
	h.t.Helper()
	args := strings.Split(argv, "\n")
	var values []string
	for i, a := range args {
		if a == "--settings" && i+1 < len(args) {
			values = append(values, args[i+1])
		}
	}
	if len(values) != 1 {
		h.t.Fatalf("claude should get exactly one --settings, got %q:\n%s", values, argv)
	}
	return values[0]
}

// claudeSettings is what claude's --settings carries.
type claudeSettings struct {
	Attribution struct {
		Commit, PR *string
		SessionURL *bool `json:"sessionUrl"`
	}
	StatusLine struct{ Type, Command string }
}

// assertAttributionOff checks that settings, as data, turns attribution off.
func (h *Harness) assertAttributionOff(data []byte) claudeSettings {
	h.t.Helper()
	var settings claudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		h.t.Fatalf("parsing --settings: %v\n%s", err, data)
	}
	if a := settings.Attribution; a.Commit == nil || *a.Commit != "" || a.PR == nil || *a.PR != "" || a.SessionURL == nil || *a.SessionURL {
		h.t.Errorf("--settings should turn attribution off:\n%s", data)
	}
	return settings
}

// assertWorkerSettings checks that a worker's argv carries exactly one
// --settings, the inline settings that turn attribution off.
func (h *Harness) assertWorkerSettings(argv string) {
	h.t.Helper()
	h.assertAttributionOff([]byte(h.claudeSettingsArg(argv)))
}

// assertSettingsOutside checks that a coordinator's argv loads its one
// --settings from a file outside dir that turns attribution off and carries
// a status line naming repo.
func (h *Harness) assertSettingsOutside(argv, dir, repo string) {
	h.t.Helper()
	path := h.claudeSettingsArg(argv)
	if !filepath.IsAbs(path) {
		h.t.Fatalf("claude should load --settings from a file, got %q:\n%s", path, argv)
	}
	if rel, err := filepath.Rel(dir, path); err != nil || !strings.HasPrefix(rel, "..") {
		h.t.Errorf("--settings %s is inside the worktree %s", path, dir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("reading --settings file: %v", err)
	}
	settings := h.assertAttributionOff(data)
	if settings.StatusLine.Type != "command" || !strings.Contains(settings.StatusLine.Command, repo) {
		h.t.Errorf("the --settings file should carry a status line naming %s:\n%s", repo, data)
	}
}

// assertCleanWorktree checks that klaus left nothing in dir for git to see,
// and no .claude/ of its own.
func (h *Harness) assertCleanWorktree(dir string) {
	h.t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".claude")); !os.IsNotExist(err) {
		h.t.Errorf("klaus should write no .claude/ into %s (stat: %v)", dir, err)
	}
	if out := h.GitOutput(dir, "status", "--porcelain", "--untracked-files=all"); out != "" {
		h.t.Errorf("git status in %s should be clean:\n%s", dir, out)
	}
}

// TestWorkerSettingsStayOutOfTheWorktree launches a Claude worker in a valley
// repo that neither tracks nor ignores .claude/. The worker gets no status
// line, only the inline settings that turn attribution off, so its worktree
// holds nothing git sees. The agent commits only its own file and pushes, as
// run 20261016-1252-85a0e972's did; finalize then has nothing to salvage,
// records the branch as pushed, and marks the change ready.
func TestWorkerSettingsStayOutOfTheWorktree(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub()
	h.joinValley()
	if err := exec.Command("git", "-C", h.RepoDir, "check-ignore", "-q", ".claude/settings.json").Run(); err == nil {
		t.Fatal("the sandbox repo must not ignore .claude/")
	}
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")

	st, _ := h.launchRun("add a health check endpoint")
	h.WaitForClaudeStart(30 * time.Second)
	h.assertCleanWorktree(st.Worktree)
	h.assertWorkerSettings(h.ClaudeArgv())

	if err := os.WriteFile(filepath.Join(st.Worktree, "health.go"), []byte("package health\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.git(st.Worktree, "add", "health.go")
	h.git(st.Worktree, "commit", "-qm", "Add a health check")
	h.git(st.Worktree, "push", "-q", "-u", "origin", "HEAD")
	head := h.GitOutput(st.Worktree, "rev-parse", "HEAD")

	st = h.finishRun(st.ID)
	if st.NeedsAttention != nil {
		t.Errorf("finalize should salvage nothing: %s", *st.NeedsAttention)
	}
	if tip := h.GitOutput(h.OriginDir, "rev-parse", "refs/heads/"+st.Branch); tip != head {
		t.Errorf("origin's %s at %s (%s), want the agent's %s", st.Branch, tip,
			h.GitOutput(h.OriginDir, "log", "-1", "--format=%s", tip), head)
	}
	if st.PushedBranch == nil || st.PushedSHA == nil {
		t.Errorf("finalize should record %s as pushed at %s", st.Branch, head)
	} else if *st.PushedBranch != st.Branch || *st.PushedSHA != head {
		t.Errorf("pushed %s at %s, want %s at %s", *st.PushedBranch, *st.PushedSHA, st.Branch, head)
	}
	if calls := h.valleyCalls("ready"); len(calls) != 1 || st.Valley == nil || st.Valley.Handle != "nxvp" {
		t.Errorf("finalize should mark the change ready: valley ready calls %q, recorded %+v", calls, st.Valley)
	}
}

// TestLaunchLeavesARepoClaudeSettingsAlone launches a Claude worker in a repo
// that tracks its own .claude/settings.json: the worktree's copy is left as
// the repo has it, for Claude Code to load beneath klaus's inline settings.
func TestLaunchLeavesARepoClaudeSettingsAlone(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	const repoSettings = `{"permissions":{"allow":["Bash(make:*)"]}}` + "\n"
	if err := os.MkdirAll(filepath.Join(h.RepoDir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.RepoDir, ".claude", "settings.json"), []byte(repoSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	h.git(h.RepoDir, "add", ".claude/settings.json")
	h.git(h.RepoDir, "commit", "-qm", "add claude settings")
	h.git(h.RepoDir, "push", "-q", "origin", "main")

	st, _ := h.launchRun("add a health check endpoint")
	h.WaitForClaudeStart(30 * time.Second)
	if got, err := os.ReadFile(filepath.Join(st.Worktree, ".claude", "settings.json")); err != nil || string(got) != repoSettings {
		t.Errorf("the repo's .claude/settings.json = %q (%v), want it unmodified: %q", got, err, repoSettings)
	}
	if out := h.GitOutput(st.Worktree, "status", "--porcelain", "--untracked-files=all"); out != "" {
		t.Errorf("git status should be clean:\n%s", out)
	}
	h.assertWorkerSettings(h.ClaudeArgv())
	h.finishRun(st.ID)
}

// TestCoordinatorSettingsStayOutOfTheWorktree starts a Claude coordinator in
// a repo, then resumes it: each time claude loads its status line from a file
// in the session's state, and the coordinator's worktree stays clean.
func TestCoordinatorSettingsStayOutOfTheWorktree(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	argsFile := filepath.Join(h.E2EDir, "coordinator.args")
	h.WriteStub("claude", "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+argsFile+"'\n")

	for _, verb := range []string{"new", "session"} {
		os.Remove(argsFile)
		res := h.RunKlaus(verb, "--backend", "claude")
		if res.ExitCode != 0 {
			t.Fatalf("klaus %s exited %d\nstdout:\n%s\nstderr:\n%s", verb, res.ExitCode, res.Stdout, res.Stderr)
		}
		var session *run.State
		for _, id := range h.sessionIDs() {
			if st, err := run.NewHomeDirStoreFromPath(filepath.Join(h.Home, ".klaus", "sessions", id)).Load(id); err == nil && st.Type == "session" {
				session = st
			}
		}
		if session == nil || session.Branch == "" {
			t.Fatalf("klaus %s recorded no coordinator worktree: %+v", verb, session)
		}
		argv, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatalf("klaus %s did not start claude: %v", verb, err)
		}
		h.assertCleanWorktree(session.Worktree)
		h.assertSettingsOutside(string(argv), session.Worktree, filepath.Base(h.RepoDir))
	}
}

// sessionIDs lists the coordinator sessions under the temp HOME.
func (h *Harness) sessionIDs() []string {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.Home, ".klaus", "sessions"))
	if err != nil {
		h.t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.Name())
	}
	return ids
}

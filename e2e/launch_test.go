//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/cmd"
	"github.com/patflynn/klaus/internal/run"
)

// TestLaunchLifecycle is the headline scenario. With a fake claude, a real git
// worktree, and a fake gh, it drives `klaus launch` end-to-end and asserts the
// full lifecycle:
//
//	(a) a real pane is created in the isolated tmux server with the agent title,
//	(b) run state is written with a branch, worktree, and pane,
//	(c) the fake claude was invoked with the expected args,
//	(d) after the agent completes, _finalize populates cost/duration/PR URL from
//	    the stub's emitted stream-json, and
//	(e) the worktree is removed and the pane is killed.
func TestLaunchLifecycle(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	const prompt = "add a health check endpoint"
	res := h.RunKlaus("launch", prompt)
	if res.ExitCode != 0 {
		t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	// Exactly one run should have been recorded.
	ids := h.RunIDs()
	if len(ids) != 1 {
		t.Fatalf("expected 1 run, got %d: %v", len(ids), ids)
	}
	runID := ids[0]

	// (b) State is written with branch/worktree/pane while the agent runs.
	st, err := h.ReadState(runID)
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	if st.Branch == "" {
		t.Error("state has empty Branch")
	}
	if st.Worktree == "" {
		t.Error("state has empty Worktree")
	}
	if st.TmuxPane == nil || *st.TmuxPane == "" {
		t.Fatal("state has no TmuxPane")
	}
	pane := *st.TmuxPane

	// The worktree should actually exist on disk during the run.
	if _, err := os.Stat(st.Worktree); err != nil {
		t.Errorf("worktree should exist during run: %v", err)
	}

	// Wait until the fake claude is actually running in the pane before we
	// assert on the live pane (the stub blocks until released).
	h.WaitForClaudeStart(30 * time.Second)

	// (a) The pane exists in the isolated server with the expected title.
	if !h.PaneExists(pane) {
		t.Fatalf("expected pane %s to exist; panes: %v", pane, h.ListPanes())
	}
	wantTitle := cmd.FormatPaneTitle(runID, "", prompt)
	if got := h.PaneTitle(pane); got != wantTitle {
		t.Errorf("pane title = %q, want %q", got, wantTitle)
	}

	// (c) claude was invoked with the expected args.
	argv := h.ClaudeArgv()
	for _, want := range []string{prompt, "-p", "--output-format", "stream-json", "--max-budget-usd"} {
		if !strings.Contains(argv, want) {
			t.Errorf("claude argv missing %q\n--- argv ---\n%s", want, argv)
		}
	}
	// With neither flag nor config set, the claude command must not carry
	// --model/--effort at all — claude's own resolution applies unchanged.
	for _, absent := range []string{"--model", "--effort"} {
		if strings.Contains(argv, absent) {
			t.Errorf("claude argv should not carry %q when unset\n--- argv ---\n%s", absent, argv)
		}
	}

	// Let the agent finish; the pipeline runs _format-stream then _finalize.
	h.ReleaseClaude()

	// (d) _finalize populates cost/duration/PR URL from the emitted log.
	final := h.WaitForState(runID, func(s *run.State) bool {
		return s.CostUSD != nil && s.TmuxPane == nil
	}, 30*time.Second)

	if final.CostUSD == nil || *final.CostUSD != 0.1234 {
		t.Errorf("CostUSD = %v, want 0.1234", final.CostUSD)
	}
	if final.DurationMS == nil || *final.DurationMS != 4242 {
		t.Errorf("DurationMS = %v, want 4242", final.DurationMS)
	}
	if final.PRURL == nil || *final.PRURL != "https://github.com/acme/widget/pull/4242" {
		t.Errorf("PRURL = %v, want the PR URL from the stub log", final.PRURL)
	}

	// (e) The worktree is removed and the pane is killed.
	if final.Worktree != "" {
		t.Errorf("Worktree should be cleared after cleanup, got %q", final.Worktree)
	}
	if _, err := os.Stat(st.Worktree); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be removed, stat err: %v", err)
	}
	waitPaneGone(t, h, pane, 30*time.Second)
}

// waitPaneGone polls until the pane disappears from the isolated server.
func waitPaneGone(t *testing.T, h *Harness, pane string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !h.PaneExists(pane) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("pane %s still alive after %s; panes: %v", pane, timeout, h.ListPanes())
}

// TestLaunchPromptFile covers the failure --prompt-file exists to prevent: a
// prompt full of backticks passed as a shell argument is mangled by the shell
// (in zsh, backticks inside double quotes are command substitution) long before
// klaus sees it. Read from a file, the prompt must reach run state — and the
// agent — byte for byte.
func TestLaunchPromptFile(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	const prompt = "Fix `ValidateToken()` in internal/auth/verify.go:87 —\n" +
		"it uses `Before()` where it should use `After()`.\n" +
		"Do not touch $HOME handling; run `go test ./...` before pushing.\n"
	promptPath := filepath.Join(h.E2EDir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte(prompt), 0o644); err != nil {
		t.Fatalf("writing prompt file: %v", err)
	}

	res := h.RunKlaus("launch", "--prompt-file", promptPath, "--budget", "7.50")
	if res.ExitCode != 0 {
		t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	ids := h.RunIDs()
	if len(ids) != 1 {
		t.Fatalf("expected 1 run, got %d: %v", len(ids), ids)
	}
	runID := ids[0]

	st, err := h.ReadState(runID)
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	if st.Prompt != prompt {
		t.Errorf("state prompt does not match the file byte for byte:\n got: %q\nwant: %q", st.Prompt, prompt)
	}
	// Everything else downstream behaves as it does for a positional prompt.
	if st.Budget == nil || *st.Budget != "7.50" {
		t.Errorf("Budget = %v, want 7.50", st.Budget)
	}
	if st.Branch == "" || st.Worktree == "" || st.TmuxPane == nil {
		t.Errorf("state incomplete: branch=%q worktree=%q pane=%v", st.Branch, st.Worktree, st.TmuxPane)
	}

	// The agent is actually briefed with the full text, backticks intact.
	h.WaitForClaudeStart(30 * time.Second)
	if argv := h.ClaudeArgv(); !strings.Contains(argv, prompt) {
		t.Errorf("claude argv missing the file prompt\n--- argv ---\n%s", argv)
	}

	h.ReleaseClaude()
	h.WaitForState(runID, func(s *run.State) bool { return s.TmuxPane == nil }, 30*time.Second)
}

// TestLaunchModelEffort covers per-launch model/effort selection: config
// defaults apply when a flag is unset, a flag overrides the config default,
// the values reach the claude argv, and the run state records what the run
// executed under.
func TestLaunchModelEffort(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	// Config sets defaults for both; the launch overrides only --effort.
	h.AmendRepoConfig(map[string]any{
		"default_agent_model":  "claude-config-model",
		"default_agent_effort": "high",
	})

	res := h.RunKlaus("launch", "add retries to the fetcher", "--effort", "low")
	if res.ExitCode != 0 {
		t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	ids := h.RunIDs()
	if len(ids) != 1 {
		t.Fatalf("expected 1 run, got %d: %v", len(ids), ids)
	}

	h.WaitForClaudeStart(30 * time.Second)
	argv := h.ClaudeArgv()
	// Config default applies for the model; the flag wins for effort.
	if !strings.Contains(argv, "--model\nclaude-config-model\n") {
		t.Errorf("claude argv missing config-default model\n--- argv ---\n%s", argv)
	}
	if !strings.Contains(argv, "--effort\nlow\n") {
		t.Errorf("claude argv missing flag-override effort\n--- argv ---\n%s", argv)
	}
	if strings.Contains(argv, "--effort\nhigh\n") {
		t.Errorf("config effort should be overridden by the flag\n--- argv ---\n%s", argv)
	}

	// Run state records what the run executed under.
	st, err := h.ReadState(ids[0])
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	if st.Model == nil || *st.Model != "claude-config-model" {
		t.Errorf("state Model = %v, want claude-config-model", st.Model)
	}
	if st.Effort == nil || *st.Effort != "low" {
		t.Errorf("state Effort = %v, want low", st.Effort)
	}

	h.ReleaseClaude()
	h.WaitForState(ids[0], func(s *run.State) bool { return s.TmuxPane == nil }, 30*time.Second)
}

// TestLaunchInvalidEffort asserts an effort outside the claude CLI's set is
// rejected up front: non-zero exit, an error naming the valid values, and no
// run recorded.
func TestLaunchInvalidEffort(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	res := h.RunKlaus("launch", "do the thing", "--effort", "turbo")
	if res.ExitCode == 0 {
		t.Fatalf("launch with invalid effort exited 0\nstdout:\n%s", res.Stdout)
	}
	for _, want := range []string{"turbo", "low", "medium", "high", "xhigh", "max"} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("stderr should mention %q, got:\n%s", want, res.Stderr)
		}
	}
	if ids := h.RunIDs(); len(ids) != 0 {
		t.Errorf("no run should have been recorded, got %v", ids)
	}
}

// TestLaunchPromptSourceErrors asserts the prompt source is unambiguous: both
// sources or neither is a clear error and no agent is launched.
func TestLaunchPromptSourceErrors(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	promptPath := filepath.Join(h.E2EDir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("do the thing\n"), 0o644); err != nil {
		t.Fatalf("writing prompt file: %v", err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"both sources", []string{"launch", "do the thing", "--prompt-file", promptPath}},
		{"no source", []string{"launch"}},
		{"missing file", []string{"launch", "--prompt-file", filepath.Join(h.E2EDir, "nope.md")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := h.RunKlaus(tc.args...)
			if res.ExitCode == 0 {
				t.Fatalf("launch %v exited 0, want a failure\nstdout:\n%s", tc.args, res.Stdout)
			}
			if !strings.Contains(res.Stderr, "prompt") {
				t.Errorf("stderr should name the problem, got:\n%s", res.Stderr)
			}
		})
	}

	if ids := h.RunIDs(); len(ids) != 0 {
		t.Errorf("no run should have been recorded, got %v", ids)
	}
}

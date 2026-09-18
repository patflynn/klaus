package cmd

import (
	"fmt"
	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/run"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The prompt file must reach the backend's stdin — locally and across the ssh
// hop (the stub forwards stdin as real ssh does) — byte for byte, never executed.
func TestPaneCommandPromptTransport(t *testing.T) {
	dir := t.TempDir()
	worktree := filepath.Join(dir, "worktree with ' quotes")
	if err := os.Mkdir(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"ssh":   "#!/bin/sh\nshift\nexec sh -c \"$1\"\n",
		"rsync": "#!/bin/sh\nexit 0\n",
		"klaus": "#!/bin/sh\nif [ \"$1\" = _format-stream ]; then cat; fi\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv(sessionIDEnv, "")
	prompt := "a 'quoted' prompt\n$(exit 17) `exit 18` #{pane_id}\n"
	promptPath := filepath.Join(dir, "prompts", "it's.md")
	if err := run.WritePromptFile(promptPath, prompt); err != nil {
		t.Fatal(err)
	}
	agent := backend.ShellCommand([]string{"cat"})
	logFile := filepath.Join(dir, "log.jsonl")
	for name, command := range map[string]string{
		"local":   buildPaneCommand(worktree, agent, promptPath, logFile, "klaus", "", "test-run"),
		"sandbox": buildSandboxPaneCommand("test-host", worktree, agent, promptPath, logFile, "klaus", "", "test-run"),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command("sh", "-c", command).CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if !strings.HasPrefix(string(out), prompt) || !strings.Contains(string(out), `"exit_code":0`) {
				t.Fatalf("transport corrupted output: %s", out)
			}
		})
	}
}

func TestClaudeLimitExitPreservesResult(t *testing.T) {
	for _, subtype := range []string{"error_max_budget_usd", "error_max_turns", "success", "error_during_execution"} {
		t.Run(subtype, func(t *testing.T) {
			log := `{"type":"result","subtype":"` + subtype + `","is_error":` + fmt.Sprint(subtype != "success") + `,"total_cost_usd":4.99,"session_id":"previous-conversation"}
{"type":"klaus_exit","exit_code":1}
`
			state, store := setupFinalizeTest(t, log)
			budget := "5.00"
			state.Budget = &budget
			got, err := finalizeFromLog(store, state)
			if err != nil {
				t.Fatal(err)
			}
			if got != subtype {
				t.Fatalf("subtype %q replaced with %q", subtype, got)
			}
			if (state.FailureReason != nil) != (subtype == "error_during_execution") {
				t.Fatalf("wrong failure classification: %+v", state)
			}
			if subtype == "error_max_budget_usd" && !isBudgetExhausted(state, got) {
				t.Fatal("budget pause lost")
			}
			if state.ClaudeSessionID == nil {
				t.Fatal("resume identity lost")
			}
		})
	}
}

func TestFindResumeConversationNil(t *testing.T) {
	if got := findResumeConversation(nil); got != "" {
		t.Fatal(got)
	}
}

//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/config"
)

// TestWorkerPromptCarriesRunContract checks that every backend's worker is told
// its run ends with its turn, even when a custom .klaus/prompt.md replaces the
// default template.
func TestWorkerPromptCarriesRunContract(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "agy"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			if err := os.WriteFile(filepath.Join(h.RepoDir, ".klaus", "prompt.md"), []byte("House rules for {{.RunID}}.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			h.WriteStub(kind, h.claudeStubScript())
			res := h.RunKlaus("launch", "--backend", kind, "small task")
			if res.ExitCode != 0 {
				t.Fatalf("launch: %s%s", res.Stdout, res.Stderr)
			}
			h.WaitForClaudeStart(30 * time.Second)

			argv := strings.Split(h.ClaudeArgv(), "\n")
			var system string
			for i, a := range argv {
				switch {
				case kind == "claude" && a == "--append-system-prompt-file" && i+1 < len(argv):
					data, err := os.ReadFile(argv[i+1])
					if err != nil {
						t.Fatal(err)
					}
					system = string(data)
				case kind == "codex" && strings.HasPrefix(a, "developer_instructions="):
					if err := json.Unmarshal([]byte(strings.TrimPrefix(a, "developer_instructions=")), &system); err != nil {
						t.Fatal(err)
					}
				case kind == "agy" && a == "--print" && i+1 < len(argv):
					// argv is recorded one line per entry; the prompt spans the rest.
					system = strings.Join(argv[i+1:], "\n")
				}
			}
			if !strings.Contains(system, "House rules for ") || !strings.Contains(system, config.WorkerRules) {
				t.Fatalf("%s worker system prompt lacks the custom template or the run contract:\n%s", kind, system)
			}
			releaseBackendWorkers(t, h)
		})
	}
}

// TestFinalizeNamesLeftoverBackgroundWork launches a worker that leaves
// unfinished work and exits without a PR, once with a background process still
// running in its worktree. The needs-attention note must name that process, and
// must not blame the pane's own shell, which also sits in the worktree.
func TestFinalizeNamesLeftoverBackgroundWork(t *testing.T) {
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("leftover-process detection reads /proc")
	}
	for _, background := range []bool{true, false} {
		t.Run(fmt.Sprintf("background=%t", background), func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			pidFile := filepath.Join(h.E2EDir, "bg.pid")
			bg := ""
			if background {
				bg = fmt.Sprintf("sleep 120 </dev/null >/dev/null 2>&1 &\necho $! > %q\n", pidFile)
				t.Cleanup(func() {
					if b, err := os.ReadFile(pidFile); err == nil {
						if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
							syscall.Kill(pid, syscall.SIGKILL)
						}
					}
				})
			}
			// Block until released like the default stub: a worker that exits
			// before launch saves its state finalizes nothing.
			h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.01,"duration_ms":10,"session_id":"11111111-2222-4333-8444-555555555555"}` + "\n")
			h.WriteStub("claude", h.claudeStubScript()+"echo wip > unfinished.txt\n"+bg)

			res := h.RunKlaus("launch", "build it")
			if res.ExitCode != 0 {
				t.Fatalf("launch: %s%s", res.Stdout, res.Stderr)
			}
			ids := h.RunIDs()
			if len(ids) != 1 {
				t.Fatalf("runs: %v", ids)
			}
			h.WaitForClaudeStart(30 * time.Second)
			releaseBackendWorkers(t, h)
			st, err := h.ReadState(ids[0])
			if err != nil {
				t.Fatal(err)
			}
			if st.NeedsAttention == nil {
				t.Fatalf("salvaged run not marked needs-attention: %+v", st)
			}
			if background {
				const want = "background work still running (sleep)"
				if st.FailureReason == nil || *st.FailureReason != want {
					t.Fatalf("failure reason = %v, want %q", st.FailureReason, want)
				}
				if !strings.HasPrefix(*st.NeedsAttention, want+"; ") {
					t.Fatalf("needs-attention = %q, want it to start with %q", *st.NeedsAttention, want)
				}
			} else {
				if st.FailureReason != nil {
					t.Fatalf("failure reason = %q, want none", *st.FailureReason)
				}
				if !strings.HasPrefix(*st.NeedsAttention, "no_pr; ") {
					t.Fatalf("needs-attention = %q, want no_pr", *st.NeedsAttention)
				}
			}
		})
	}
}

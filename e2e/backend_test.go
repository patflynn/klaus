//go:build e2e

package e2e

import (
	"fmt"
	"github.com/patflynn/klaus/internal/run"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackendWorkerLifecycle(t *testing.T) {
	for _, kind := range []string{"codex", "agy"} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failure=%t", kind, failed), func(t *testing.T) {
				t.Parallel()
				h := NewHarness(t)
				output := `{"type":"thread.started","thread_id":"codex-thread"}
{"type":"item.completed","item":{"type":"agent_message","text":"https://github.com/acme/widget/pull/42"}}
{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":3}}`
				sessionID := "codex-thread"
				if kind == "agy" {
					sessionID = "agy-conversation"
					output = `{"event":"init","conversation_id":"agy-conversation","init":{"model":"test"}}
{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"Done","state":"DONE"}}
{"event":"result","result":{"conversation_id":"agy-conversation","status":"SUCCESS","response":"https://github.com/acme/widget/pull/42","duration_seconds":1}}`
				}
				status := 0
				if failed {
					output = ""
					status = 7
				}
				h.WriteStub(kind, h.claudeStubScript()+fmt.Sprintf("\nexit %d\n", status))
				if err := os.WriteFile(filepath.Join(h.E2EDir, "claude.output.jsonl"), []byte(output+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				res := h.RunKlaus("launch", "--backend", kind, "--model", "test-model", "task 'quoted' $(false)")
				if res.ExitCode != 0 {
					t.Fatalf("launch: %s%s", res.Stdout, res.Stderr)
				}
				ids := h.RunIDs()
				if len(ids) != 1 {
					t.Fatalf("runs: %v", ids)
				}
				h.WaitForClaudeStart(15 * time.Second)
				argv := h.ClaudeArgv()
				if kind == "agy" && !strings.Contains(argv, "--add-dir\n.\n") {
					t.Fatal("agy worker missing explicit workspace")
				}
				if strings.Contains(argv, "--max-budget-usd") || !strings.Contains(argv, "test-model") || !strings.Contains(argv, "task 'quoted' $(false)") {
					t.Fatalf("bad args: %s", argv)
				}
				releaseBackendWorkers(t, h)
				st := h.WaitForState(ids[0], func(s *run.State) bool { return s.DurationMS != nil }, 15*time.Second)
				if st.Backend != kind || st.Budget != nil || st.CostUSD != nil {
					t.Fatalf("incorrect backend/budget/cost: %+v", st)
				}
				if failed {
					if st.FailureReason == nil || !strings.Contains(*st.FailureReason, "status 7") {
						t.Fatalf("missing failure: %+v", st)
					}
				} else {
					if st.FailureReason != nil || st.PRURL == nil || *st.PRURL != "https://github.com/acme/widget/pull/42" {
						t.Fatalf("incorrect completion: %+v", st)
					}
					if st.BackendSessionID == nil || *st.BackendSessionID != sessionID {
						t.Fatalf("lost conversation identity: %+v", st)
					}
					if st.ClaudeSessionID != nil {
						t.Fatal("non-Claude identity stored as Claude")
					}
				}
			})
		}
	}
}

func TestCoordinatorBackendSelection(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "agy"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			argsFile := filepath.Join(h.E2EDir, "coordinator.args")
			h.WriteStub(kind, fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n", argsFile))
			res := h.RunKlausIn(h.E2EDir, "new", "--backend", kind, "--agent-backend", "agy")
			if res.ExitCode != 0 {
				t.Fatalf("coordinator: %s%s", res.Stdout, res.Stderr)
			}
			entries, err := os.ReadDir(filepath.Join(h.Home, ".klaus", "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			var session *run.State
			for _, e := range entries {
				store := run.NewHomeDirStoreFromPath(filepath.Join(h.Home, ".klaus", "sessions", e.Name()))
				st, err := store.Load(e.Name())
				if err == nil && st.Type == "session" {
					session = st
					break
				}
			}
			if session == nil || session.Backend != kind || session.AgentBackend != "agy" {
				t.Fatalf("session selection not saved: %+v", session)
			}
			argv, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "agy" && !strings.Contains(string(argv), "--add-dir\n.\n") {
				t.Fatal("agy coordinator missing explicit workspace")
			}
			if !strings.Contains(string(argv), "klaus launch") {
				t.Fatalf("coordinator instructions missing: %s", argv)
			}
			// Resume must keep the saved backend, even though the configured default is Claude.
			res = h.RunKlausIn(h.E2EDir, "session")
			if res.ExitCode != 0 {
				t.Fatalf("resume: %s%s", res.Stdout, res.Stderr)
			}
			argv, err = os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "codex" && !strings.Contains(string(argv), "resume\n--last") {
				t.Fatalf("not resuming codex: %s", argv)
			}
			// The worker selection is independent of the coordinator and is
			// read from saved session state by a separate process (as in CI dispatch).
			h.SessionID = session.ID
			h.WriteStub("agy", h.claudeStubScript())
			res = h.RunKlaus("launch", "worker selected by session")
			if res.ExitCode != 0 {
				t.Fatalf("worker launch: %s%s", res.Stdout, res.Stderr)
			}
			h.WaitForClaudeStart(15 * time.Second)
			found := false
			for _, id := range h.RunIDs() {
				st, err := h.ReadState(id)
				if err == nil && st.Type != "session" {
					found = true
					if st.Backend != "agy" {
						t.Fatalf("worker backend = %s", st.Backend)
					}
				}
			}
			if !found {
				t.Fatal("worker state missing")
			}
			releaseBackendWorkers(t, h)

		})
	}
}

func TestBackendDefaultsAndValidation(t *testing.T) {
	for _, kind := range []string{"codex", "agy"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.AmendRepoConfig(map[string]any{
				"default_budget":        "",
				"default_agent_backend": kind,
				"default_agent_model":   "claude-only-model",
				"default_agent_effort":  "max",
				"backends":              map[string]any{kind: map[string]any{"model": "backend-model", "effort": "medium"}},
			})
			for _, flags := range [][]string{{"--budget", "2"}, {"--replay"}, {"--effort", "max"}, {"--backend", "typo"}} {
				args := append([]string{"launch", "task"}, flags...)
				res := h.RunKlaus(args...)
				if res.ExitCode == 0 {
					t.Fatalf("expected validation error for %v", flags)
				}
				if len(h.RunIDs()) != 0 {
					t.Fatalf("invalid options created runs: %v", flags)
				}
			}
			h.WriteStub(kind, h.claudeStubScript())
			res := h.RunKlaus("launch", "task")
			if res.ExitCode != 0 {
				t.Fatalf("launch: %s%s", res.Stdout, res.Stderr)
			}
			if strings.Contains(res.Stderr, "default_budget does not apply") {
				t.Fatal("warned about an empty default budget")
			}
			h.WaitForClaudeStart(15 * time.Second)
			args := h.ClaudeArgv()
			if strings.Contains(args, "claude-only-model") || strings.Contains(args, "\nmax\n") || !strings.Contains(args, "backend-model") {
				t.Fatalf("wrong backend defaults: %s", args)
			}
			releaseBackendWorkers(t, h)
		})
	}
}

func TestCoordinatorFailureStillTearsDown(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "agy"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.WriteStub(kind, "#!/bin/sh\nexit 7\n")
			res := h.RunKlausIn(h.E2EDir, "new", "--backend", kind)
			if res.ExitCode == 0 || !strings.Contains(res.Stderr, "exit status 7") {
				t.Fatalf("missing exit error: %+v", res)
			}
			if !strings.Contains(res.Stdout, "No agents running.") || !strings.Contains(res.Stdout, "To clean up:") {
				t.Fatalf("teardown skipped: %+v", res)
			}
			if panes := h.ListPanes(); len(panes) != 1 {
				t.Fatalf("orphaned dashboard: %v", panes)
			}
		})
	}
}

// Wait through the entire finalizer, not just its first state update: it still
// syncs git refs and removes the worktree after recording duration. Letting
// TempDir cleanup race those writes can fail with "directory not empty".
func releaseBackendWorkers(t *testing.T, h *Harness) {
	t.Helper()
	var workers []*run.State
	for _, id := range h.RunIDs() {
		st, err := h.ReadState(id)
		if err != nil {
			t.Fatal(err)
		}
		if st.Type != "session" && st.TmuxPane != nil {
			workers = append(workers, st)
		}
	}
	if len(workers) == 0 {
		t.Fatal("no worker panes to release")
	}
	if err := os.WriteFile(filepath.Join(h.E2EDir, "claude.release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, st := range workers {
		h.WaitForState(st.ID, func(s *run.State) bool { return s.DurationMS != nil && s.TmuxPane == nil && s.Worktree == "" }, 30*time.Second)
		waitPaneGone(t, h, *st.TmuxPane, 30*time.Second)
	}
}

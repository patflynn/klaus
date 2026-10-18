package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
)

// A local worker of every backend runs cut off from its pane's tmux server: no
// $TMUX or $TMUX_PANE, a TMUX_TMPDIR of its own, and the server's socket only
// in tmux.SocketEnv, for klaus. _finalize runs after it in the pane's shell
// and keeps $TMUX. Runs the real pane command with stand-in binaries; nothing
// here runs tmux.
func TestWorkerRunsOutOfTmuxReach(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "shared.sock")
	paneTMUX := sock + ",4242,0"
	t.Setenv("TMUX", paneTMUX)
	t.Setenv("TMUX_PANE", "%7")
	t.Setenv(tmux.SocketEnv, "")
	t.Setenv(sessionIDEnv, "")

	finalized := filepath.Join(dir, "finalize.tmux")
	report := `printf '%s\n' "TMUX=${TMUX-unset}" "TMUX_PANE=${TMUX_PANE-unset}" "TMUX_TMPDIR=${TMUX_TMPDIR-unset}" "` + tmux.SocketEnv + `=${` + tmux.SocketEnv + `-unset}" "KLAUS_BACKEND=${KLAUS_BACKEND-unset}"`
	stubs := map[string]string{
		"klaus": "#!/bin/sh\ncase \"$1\" in\n_format-stream) cat ;;\n_finalize) printf '%s' \"${TMUX-unset}\" > " + shellQuote(finalized) + " ;;\nesac\n",
	}
	kinds := []backend.Kind{backend.Claude, backend.Codex, backend.Agy}
	for _, kind := range kinds {
		stubs[string(kind)] = "#!/bin/sh\n" + report + "\n"
	}
	for name, script := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	worktree := filepath.Join(dir, "worktree")
	promptPath := filepath.Join(dir, "prompt.md")
	for _, p := range []error{os.Mkdir(worktree, 0o700), run.WritePromptFile(promptPath, "go")} {
		if p != nil {
			t.Fatal(p)
		}
	}
	o := backend.Options{SystemPrompt: "sys", SystemPromptFile: filepath.Join(dir, "sys.md"), Prompt: "go", RunID: "r", Budget: "5"}

	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			os.Remove(finalized)
			tmuxDir := filepath.Join(dir, "tmux dirs", string(kind))
			command, err := agentPaneCommand(kind, o, promptPath, "", worktree, filepath.Join(dir, "log.jsonl"), "", "r", tmuxDir)
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("sh", "-c", command).CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			for _, want := range []string{"TMUX=unset", "TMUX_PANE=unset", "TMUX_TMPDIR=" + tmuxDir, tmux.SocketEnv + "=" + sock, "KLAUS_BACKEND=" + string(kind)} {
				if !strings.Contains(string(out), want+"\n") {
					t.Errorf("worker environment lacks %q:\n%s", want, out)
				}
			}
			if got, _ := os.ReadFile(finalized); string(got) != paneTMUX {
				t.Errorf("_finalize saw TMUX=%q, want the pane's %q", got, paneTMUX)
			}
		})
	}

	// No directory would leave TMUX_TMPDIR empty: tmux's default, the
	// shared server's likely home.
	if _, err := agentPaneCommand(backend.Claude, o, promptPath, "", worktree, "/l.jsonl", "", "r", ""); err == nil {
		t.Error("a local worker with no tmux directory got a pane command")
	}
}

func TestAgentsSessionName(t *testing.T) {
	t.Run("uses KLAUS_SESSION_ID", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "session-20260907-1241-c0da2e6d")
		if got, want := agentsSessionName(), "klaus-agents-session-20260907-1241-c0da2e6d"; got != want {
			t.Errorf("agentsSessionName() = %q, want %q", got, want)
		}
	})

	t.Run("replaces tmux target separators", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "a.b:c")
		if got, want := agentsSessionName(), "klaus-agents-a-b-c"; got != want {
			t.Errorf("agentsSessionName() = %q, want %q", got, want)
		}
	})

	t.Run("falls back when unset", func(t *testing.T) {
		t.Setenv(sessionIDEnv, "")
		if got, want := agentsSessionName(), "klaus-agents"; got != want {
			t.Errorf("agentsSessionName() = %q, want %q", got, want)
		}
	})
}

func TestKillEmptyAgentsSession(t *testing.T) {
	t.Run("kills a session whose panes are all idle", func(t *testing.T) {
		tc := isolatedTmux(t)
		session := agentsSessionName()
		tc.panes = map[string][]string{session: {"%1", "%2"}}
		store := newFakeStore()

		out := captureStdout(t, func() {
			killEmptyAgentsSession(context.Background(), store, tc)
		})

		if len(tc.killedSessions) != 1 || tc.killedSessions[0] != session {
			t.Fatalf("expected KillSession(%q), got %v", session, tc.killedSessions)
		}
		if !contains(out, "killed agents tmux session "+session) {
			t.Errorf("expected kill message, got: %s", out)
		}
	})

	t.Run("keeps a session with a running pane even when the store is empty", func(t *testing.T) {
		tc := isolatedTmux(t)
		session := agentsSessionName()
		tc.panes = map[string][]string{session: {"%1", "%2"}}
		tc.runningPanes = map[string]bool{"%2": true}
		// The store knows nothing about the panes — exactly the state a unit
		// test's fake store is in (issue #295). tmux must still win.
		store := newFakeStore()

		out := captureStdout(t, func() {
			killEmptyAgentsSession(context.Background(), store, tc)
		})

		if len(tc.killedSessions) != 0 {
			t.Fatalf("expected no KillSession calls, got %v", tc.killedSessions)
		}
		if !contains(out, "keeping agents tmux session "+session+" (pane %2 still running)") {
			t.Errorf("expected skip message naming the live pane, got: %s", out)
		}
	})

	t.Run("keeps a session whose panes are still held by a run", func(t *testing.T) {
		tc := isolatedTmux(t)
		session := agentsSessionName()
		tc.panes = map[string][]string{session: {"%1"}}
		pane := "%1"
		store := newFakeStore(&run.State{ID: "run-1", TmuxPane: &pane})

		captureStdout(t, func() {
			killEmptyAgentsSession(context.Background(), store, tc)
		})

		if len(tc.killedSessions) != 0 {
			t.Fatalf("expected no KillSession calls, got %v", tc.killedSessions)
		}
	})

	t.Run("no-op when the session does not exist", func(t *testing.T) {
		tc := isolatedTmux(t)
		store := newFakeStore()

		killEmptyAgentsSession(context.Background(), store, tc)

		if len(tc.killedSessions) != 0 {
			t.Fatalf("expected no KillSession calls, got %v", tc.killedSessions)
		}
	})
}

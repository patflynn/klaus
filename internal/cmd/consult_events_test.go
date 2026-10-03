package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
)

// consultEnv runs the real klaus binary with stub backends first on PATH, an
// isolated HOME, and (when sessionID is non-empty) a Klaus session.
type consultEnv struct {
	bin, home, stubs, sessionID string
}

func newConsultEnv(t *testing.T, bin, sessionID string) *consultEnv {
	t.Helper()
	e := &consultEnv{bin: bin, home: t.TempDir(), stubs: t.TempDir(), sessionID: sessionID}
	if sessionID != "" {
		if err := os.MkdirAll(e.sessionDir(), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *consultEnv) sessionDir() string {
	return filepath.Join(e.home, ".klaus", "sessions", e.sessionID)
}

func (e *consultEnv) stub(t *testing.T, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.stubs, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (e *consultEnv) command(args ...string) *exec.Cmd {
	c := exec.Command(e.bin, append([]string{"consult"}, args...)...)
	c.Env = append(os.Environ(),
		"HOME="+e.home,
		"PATH="+e.stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		sessionIDEnv+"="+e.sessionID,
		"KLAUS_BACKEND=agy",
	)
	return c
}

func (e *consultEnv) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := e.command(args...).CombinedOutput()
	return string(out), err
}

func (e *consultEnv) events(t *testing.T) []event.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.sessionDir(), "events.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var evts []event.Event
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var evt event.Event
		if err := json.Unmarshal(line, &evt); err != nil {
			t.Fatalf("bad event %s: %v", line, err)
		}
		evts = append(evts, evt)
	}
	return evts
}

// pairs checks that evts holds exactly one consult:started and one
// consult:completed per ID, and returns the completions keyed by backend.
func consultPairs(t *testing.T, evts []event.Event) map[string]event.Event {
	t.Helper()
	started := map[string]event.Event{}
	completed := map[string]event.Event{}
	for _, evt := range evts {
		if !strings.HasPrefix(evt.RunID, "consult-") {
			t.Fatalf("event without consult ID: %+v", evt)
		}
		var m map[string]event.Event
		switch evt.Type {
		case event.ConsultStarted:
			m = started
		case event.ConsultCompleted:
			m = completed
		default:
			t.Fatalf("unexpected event: %+v", evt)
		}
		if _, dup := m[evt.RunID]; dup {
			t.Fatalf("duplicate %s for %s", evt.Type, evt.RunID)
		}
		m[evt.RunID] = evt
	}
	byBackend := map[string]event.Event{}
	for id, s := range started {
		c, ok := completed[id]
		if !ok {
			t.Fatalf("consult %s started without completing: %+v", id, evts)
		}
		for _, k := range []string{"backend", "model", "effort", "role", "thread", "dir", "panel", "prompt"} {
			if _, ok := s.Data[k]; !ok {
				t.Errorf("consult:started missing %q: %+v", k, s.Data)
			}
			if c.Data[k] != s.Data[k] {
				t.Errorf("%s differs: started %v, completed %v", k, s.Data[k], c.Data[k])
			}
		}
		for _, k := range []string{"duration_ms", "success"} {
			if _, ok := c.Data[k]; !ok {
				t.Errorf("consult:completed missing %q: %+v", k, c.Data)
			}
		}
		byBackend[c.Data["backend"].(string)] = c
	}
	if len(completed) != len(started) {
		t.Fatalf("unmatched completions: %+v", evts)
	}
	return byBackend
}

func TestConsultEventsEndToEnd(t *testing.T) {
	bin := klausBinary(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("success", func(t *testing.T) {
		e := newConsultEnv(t, bin, "session-consult-events")
		e.stub(t, "codex", "cat >/dev/null\necho an answer\n")
		promptFile := filepath.Join(t.TempDir(), "q.md")
		long := strings.Repeat("x", 200)
		if err := os.WriteFile(promptFile, []byte("\n  Review the plan "+long+"\nsecond line\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := e.run(t, "--backend", "codex", "--model", "gpt-9", "--effort", "high", "--role", "critic", "--dir", dir, "--prompt-file", promptFile); err != nil {
			t.Fatalf("consult: %s, %v", out, err)
		}
		c := consultPairs(t, e.events(t))["codex"]
		want := map[string]interface{}{"model": "gpt-9", "effort": "high", "role": "critic", "thread": "", "dir": dir, "panel": false, "prompt_file": promptFile, "success": true}
		for k, v := range want {
			if c.Data[k] != v {
				t.Errorf("%s = %v, want %v", k, c.Data[k], v)
			}
		}
		prompt := c.Data["prompt"].(string)
		if !strings.HasPrefix(prompt, "Review the plan xxx") || strings.Contains(prompt, "second") || len([]rune(prompt)) != 120 {
			t.Errorf("prompt excerpt = %q", prompt)
		}
		if _, ok := c.Data["error"]; ok {
			t.Errorf("successful consult has error: %+v", c.Data)
		}
	})

	t.Run("failure", func(t *testing.T) {
		e := newConsultEnv(t, bin, "session-consult-events")
		e.stub(t, "codex", "echo secret model output\necho backend broke >&2\nexit 7\n")
		if out, err := e.run(t, "--backend", "codex", "--dir", dir, "question"); err == nil {
			t.Fatalf("failing backend succeeded: %s", out)
		}
		c := consultPairs(t, e.events(t))["codex"]
		errMsg, _ := c.Data["error"].(string)
		if c.Data["success"] != false || !strings.Contains(errMsg, "exit status 7") || strings.Contains(errMsg, "secret") {
			t.Errorf("failure event: %+v", c.Data)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		e := newConsultEnv(t, bin, "session-consult-events")
		e.stub(t, "codex", "sleep 30\n")
		start := time.Now()
		if out, err := e.run(t, "--backend", "codex", "--dir", dir, "--timeout", "1s", "question"); err == nil {
			t.Fatalf("slow backend succeeded: %s", out)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("timeout not enforced")
		}
		c := consultPairs(t, e.events(t))["codex"]
		if c.Data["success"] != false || c.Data["error"] != "timeout after 1s" {
			t.Errorf("timeout event: %+v", c.Data)
		}
	})

	t.Run("interrupted", func(t *testing.T) {
		e := newConsultEnv(t, bin, "session-consult-events")
		e.stub(t, "codex", "sleep 30\n")
		c := e.command("--backend", "codex", "--dir", dir, "question")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for len(e.events(t)) == 0 {
			if time.Now().After(deadline) {
				_ = c.Process.Kill()
				t.Fatal("consult:started never appeared")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := c.Process.Signal(syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = c.Process.Kill()
			t.Fatal("consult did not exit after SIGINT")
		}
		got := consultPairs(t, e.events(t))["codex"]
		if got.Data["success"] != false || got.Data["error"] != "interrupted" {
			t.Errorf("interrupt event: %+v", got.Data)
		}
	})

	t.Run("panel", func(t *testing.T) {
		e := newConsultEnv(t, bin, "session-consult-events")
		e.stub(t, "codex", "cat >/dev/null\necho codex answer\n")
		e.stub(t, "claude", "cat >/dev/null\necho claude answer\n")
		if out, err := e.run(t, "--panel", "--dir", dir, "question"); err != nil {
			t.Fatalf("panel: %s, %v", out, err)
		}
		evts := e.events(t)
		byBackend := consultPairs(t, evts)
		if len(evts) != 4 || len(byBackend) != 2 || byBackend["codex"].RunID == byBackend["claude"].RunID {
			t.Fatalf("panel events: %+v", evts)
		}
		for _, c := range byBackend {
			if c.Data["panel"] != true || c.Data["success"] != true {
				t.Errorf("panel member: %+v", c.Data)
			}
		}
	})

	t.Run("outside session", func(t *testing.T) {
		e := newConsultEnv(t, bin, "")
		e.stub(t, "codex", "cat >/dev/null\necho an answer\n")
		if out, err := e.run(t, "--backend", "codex", "--dir", dir, "question"); err != nil {
			t.Fatalf("consult: %s, %v", out, err)
		}
		if _, err := os.Stat(filepath.Join(e.home, ".klaus")); !os.IsNotExist(err) {
			t.Errorf("consult outside a session wrote state: %v", err)
		}
	})
}

func TestWatchFormatsConsultEvents(t *testing.T) {
	data := map[string]interface{}{"backend": "claude", "model": "claude-opus-5-5", "effort": "max", "role": "critic", "thread": "", "repo": "cosmo", "panel": false, "prompt": "Critique the plan"}
	started := eventSummary(event.Event{Type: event.ConsultStarted, Data: data})
	if want := "claude/claude-opus-5-5 max critic repo=cosmo: Critique the plan"; started != want {
		t.Errorf("started = %q, want %q", started, want)
	}
	failed := map[string]interface{}{"duration_ms": float64(1000), "success": false, "error": "timeout after 1s"}
	for k, v := range data {
		failed[k] = v
	}
	line := formatEvent(event.Event{RunID: "consult-20261003-0820-abcd1234", Type: event.ConsultCompleted, Data: failed})
	for _, want := range []string{"consult-20261003-0820-abcd1234", "claude/claude-opus-5-5", "(1000ms)", "failed: timeout after 1s"} {
		if !strings.Contains(line, want) {
			t.Errorf("completed line %q missing %q", line, want)
		}
	}
}

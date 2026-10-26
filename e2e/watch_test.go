//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newCoordinatorSession runs the real `klaus new --backend kind` with a stub
// coordinator that saves its system prompt and exits, points the harness at
// the new session, and returns the prompt.
func (h *Harness) newCoordinatorSession(kind string) string {
	h.t.Helper()
	promptFile := filepath.Join(h.E2EDir, kind+".prompt")
	// Claude takes the prompt in --append-system-prompt, agy in
	// --prompt-interactive, Codex in -c developer_instructions=.
	h.WriteStub(kind, fmt.Sprintf(`#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    --append-system-prompt|--prompt-interactive) printf '%%s' "$2" > %[1]q; shift ;;
    developer_instructions=*) printf '%%s' "$1" > %[1]q ;;
  esac
  shift
done
`, promptFile))
	before := map[string]bool{}
	if _, err := os.Stat(filepath.Join(h.Home, ".klaus", "sessions")); err == nil {
		for _, id := range h.sessionIDs() {
			before[id] = true
		}
	}
	res := h.RunKlausIn(h.E2EDir, "new", "--backend", kind)
	if res.ExitCode != 0 {
		h.t.Fatalf("klaus new --backend %s exited %d\nstdout:\n%s\nstderr:\n%s", kind, res.ExitCode, res.Stdout, res.Stderr)
	}
	for _, id := range h.sessionIDs() {
		if !before[id] {
			h.SessionID = id
		}
	}
	if before[h.SessionID] {
		h.t.Fatalf("klaus new made no session (sessions: %v)", h.sessionIDs())
	}
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		h.t.Fatalf("the %s coordinator got no prompt: %v", kind, err)
	}
	return string(prompt)
}

func (h *Harness) eventsFile() string {
	return filepath.Join(h.Home, ".klaus", "sessions", h.SessionID, "events.jsonl")
}

// emitMerged appends a pr:merged event for PR n to the session's event log
// through the real binary.
func (h *Harness) emitMerged(n int) {
	h.t.Helper()
	res := h.RunKlaus("_event", "--run-id", fmt.Sprint(n), "--type", "pr:merged", "--data", fmt.Sprintf(`{"pr_number":"%d"}`, n))
	if res.ExitCode != 0 {
		h.t.Fatalf("klaus _event exited %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// exited reports whether p has exited.
func (p *klausProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// waitOpened waits until p has path open, which a streaming watch does just
// before it seeks to the end of the file and starts following it.
func (p *klausProcess) waitOpened(path string) {
	p.t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		p.t.Fatal(err)
	}
	fdDir := fmt.Sprintf("/proc/%d/fd", p.cmd.Process.Pid)
	p.waitFor(func() bool {
		fds, _ := os.ReadDir(fdDir)
		for _, fd := range fds {
			if target, _ := os.Readlink(filepath.Join(fdDir, fd.Name())); target == real {
				return true
			}
		}
		return false
	}, "open "+path)
	time.Sleep(200 * time.Millisecond)
}

// stop sends p SIGTERM and returns its outcome.
func (p *klausProcess) stop() RunResult {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		p.t.Fatal(err)
	}
	return p.wait()
}

// watchOnce runs a `klaus watch --once` command line as printed on a next:
// line, waits up to 30s for it to exit 0, and returns the PR numbers of the
// pr:merged events it printed and the command on its next: line.
func (h *Harness) watchOnce(cmdline string) (prs []string, next string) {
	h.t.Helper()
	p := h.startKlaus(nil, strings.Fields(strings.TrimPrefix(cmdline, "klaus "))...)
	p.waitFor(p.exited, "exit")
	res := p.wait()
	if res.ExitCode != 0 {
		h.t.Fatalf("%s exited %d\nstdout:\n%s\nstderr:\n%s", cmdline, res.ExitCode, res.Stdout, res.Stderr)
	}
	return parseWatchBatch(h.t, cmdline, res.Stdout)
}

var mergedLine = regexp.MustCompile(`pr:merged +PR #(\d+) merged$`)

func parseWatchBatch(t *testing.T, cmdline, out string) (prs []string, next string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "next: klaus watch --once --after ") {
		t.Fatalf("%s: last line %q is not a next: line\n%s", cmdline, last, out)
	}
	for _, l := range lines[:len(lines)-1] {
		m := mergedLine.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("%s printed %q, not a pr:merged event\n%s", cmdline, l, out)
		}
		prs = append(prs, m[1])
	}
	return prs, strings.TrimPrefix(last, "next: ")
}

// TestAgyCoordinatorWatchesWithOneShotWatches follows the event channel
// pattern the agy coordinator's prompt gives it, with the real binary: start
// the prompt's one-shot watch, and when it exits, start the command on its
// next: line. An event that lands before the first watch starts, two that land
// between two watches, and one that lands while a watch waits are each
// printed once.
func TestAgyCoordinatorWatchesWithOneShotWatches(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	prompt := h.newCoordinatorSession("agy")
	if strings.Contains(prompt, "Monitor") {
		t.Fatalf("the agy coordinator's prompt mentions Monitor:\n%s", prompt)
	}
	for _, want := range []string{"run_command", "only when they exit", "as a background task", "next:"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the agy coordinator's prompt doesn't say %q", want)
		}
	}
	m := regexp.MustCompile("start `(klaus watch --once --after 0)` as a background task").FindStringSubmatch(prompt)
	if m == nil {
		t.Fatalf("the agy coordinator's prompt has no first watch from the start of the new session's events:\n%s", prompt)
	}

	// The coordinator hasn't armed its watch yet when PR 1 merges.
	h.emitMerged(1)
	prs, next := h.watchOnce(m[1])
	if strings.Join(prs, " ") != "1" {
		t.Fatalf("the first watch printed PRs %v, want [1]", prs)
	}

	// Two events between two watches: the next one prints both, and not PR 1.
	h.emitMerged(2)
	h.emitMerged(3)
	prs, next = h.watchOnce(next)
	if strings.Join(prs, " ") != "2 3" {
		t.Fatalf("the second watch printed PRs %v, want [2 3]", prs)
	}

	// The third waits: it has nothing to print until PR 4 merges.
	p := h.startKlaus(nil, strings.Fields(strings.TrimPrefix(next, "klaus "))...)
	time.Sleep(time.Second)
	if p.exited() {
		res := p.wait()
		t.Fatalf("%s exited before any new event\nstdout:\n%s\nstderr:\n%s", next, res.Stdout, res.Stderr)
	}
	h.emitMerged(4)
	p.waitFor(p.exited, "exit")
	res := p.wait()
	if res.ExitCode != 0 {
		t.Fatalf("%s exited %d: %s%s", next, res.ExitCode, res.Stdout, res.Stderr)
	}
	prs, next = parseWatchBatch(t, next, res.Stdout)
	if strings.Join(prs, " ") != "4" {
		t.Fatalf("the third watch printed PRs %v, want [4]", prs)
	}

	// Its cursor is the end of the stream: a watch from it waits too, and
	// interrupted, says to start the same command again.
	p = h.startKlaus(nil, strings.Fields(strings.TrimPrefix(next, "klaus "))...)
	time.Sleep(500 * time.Millisecond)
	res = p.stop()
	if res.ExitCode != 0 || res.Stdout != "next: "+next+"\n" {
		t.Fatalf("interrupted, %s exited %d with %q, want just %q", next, res.ExitCode, res.Stdout, "next: "+next+"\n")
	}
}

// TestCoordinatorPromptsFollowTheirBackends: Claude's prompt arms klaus watch
// in Monitor; Codex's, like agy's, never mentions Monitor and gives the
// one-shot watch.
func TestCoordinatorPromptsFollowTheirBackends(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			prompt := h.newCoordinatorSession(kind)
			monitor := strings.Contains(prompt, "Command to arm in Monitor")
			oneShot := strings.Contains(prompt, "klaus watch --once --after 0")
			if kind == "claude" && (!monitor || oneShot) {
				t.Errorf("Claude's prompt: Monitor instructions %t, one-shot watch %t; want only Monitor", monitor, oneShot)
			}
			if kind == "codex" && (strings.Contains(prompt, "Monitor") || !oneShot) {
				t.Errorf("Codex's prompt: mentions Monitor %t, one-shot watch %t; want only the one-shot watch", strings.Contains(prompt, "Monitor"), oneShot)
			}
		})
	}
}

// TestWatchBeforeTheFirstEvent: a coordinator arms klaus watch as its session
// starts, before any event. klaus new makes the events file, and a watch
// started then prints the first event (#345).
func TestWatchBeforeTheFirstEvent(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.newCoordinatorSession("claude")
	if info, err := os.Stat(h.eventsFile()); err != nil || info.Size() != 0 {
		t.Fatalf("klaus new should leave an empty events file: %v %v", info, err)
	}

	w := h.startKlaus(nil, "watch")
	w.waitOpened(h.eventsFile())
	h.emitMerged(7)
	w.waitFor(func() bool {
		out, _ := w.output()
		return strings.Contains(out, "PR #7 merged")
	}, "print PR #7's merge")
	if res := w.stop(); res.ExitCode != 0 {
		t.Fatalf("watch exited %d after SIGTERM: %s", res.ExitCode, res.Stderr)
	}

	// Resuming a session that has no events file, as one from an older
	// klaus, makes it too.
	if err := os.Remove(h.eventsFile()); err != nil {
		t.Fatal(err)
	}
	if res := h.RunKlausIn(h.E2EDir, "session"); res.ExitCode != 0 {
		t.Fatalf("klaus session exited %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if _, err := os.Stat(h.eventsFile()); err != nil {
		t.Fatalf("resuming the session should make the events file: %v", err)
	}
}

// TestWatchWaitsForTheEventsFile: in a session with no events file yet, as
// one from an older klaus, watch waits for the file as long as the session
// exists, well past the 10s it used to give up after, and prints the event
// that makes the file. An unknown session ID fails at once.
func TestWatchWaitsForTheEventsFile(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.newCoordinatorSession("claude")
	if err := os.Remove(h.eventsFile()); err != nil {
		t.Fatal(err)
	}

	w := h.startKlaus(nil, "watch")
	once := h.startKlaus(nil, "watch", "--once")
	time.Sleep(11 * time.Second)
	for _, p := range []*klausProcess{w, once} {
		if p.exited() {
			res := p.wait()
			t.Fatalf("klaus %v gave up waiting for the events file: exit %d\n%s%s", p.cmd.Args[1:], res.ExitCode, res.Stdout, res.Stderr)
		}
	}

	h.emitMerged(8)
	w.waitFor(func() bool {
		out, _ := w.output()
		return strings.Contains(out, "PR #8 merged")
	}, "print PR #8's merge")
	if res := w.stop(); res.ExitCode != 0 {
		t.Fatalf("watch exited %d after SIGTERM: %s", res.ExitCode, res.Stderr)
	}
	once.waitFor(once.exited, "exit")
	res := once.wait()
	if prs, _ := parseWatchBatch(t, "klaus watch --once", res.Stdout); res.ExitCode != 0 || strings.Join(prs, " ") != "8" {
		t.Fatalf("watch --once exited %d with PRs %v, want 0 with [8]\n%s%s", res.ExitCode, prs, res.Stdout, res.Stderr)
	}

	start := time.Now()
	res = h.RunKlaus("watch", "--session-id", "session-unknown")
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "unknown session session-unknown") || time.Since(start) > 5*time.Second {
		t.Fatalf("watch on an unknown session exited %d after %s: %s", res.ExitCode, time.Since(start), res.Stderr)
	}
}

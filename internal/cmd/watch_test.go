package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
)

// TestWatchFilterMatches is a pure unit test for the genuinely-tricky filter
// helper. Everything else about 'klaus watch' is exercised via the real
// binary integration test below.
func TestWatchFilterMatches(t *testing.T) {
	cases := []struct {
		name      string
		include   []string
		exclude   []string
		eventType string
		want      bool
	}{
		{
			name:      "empty include uses default filter; type in default included",
			eventType: event.AgentPRCreated,
			want:      true,
		},
		{
			name:      "empty include uses default filter; type not in default excluded",
			eventType: event.AgentStarted,
			want:      false,
		},
		{
			name:      "explicit include narrows to listed types",
			include:   []string{event.PRMerged},
			eventType: event.PRMerged,
			want:      true,
		},
		{
			name:      "explicit include excludes everything else",
			include:   []string{event.PRMerged},
			eventType: event.AgentPRCreated,
			want:      false,
		},
		{
			name:      "filter-out subtracts from default",
			exclude:   []string{event.PRApproved},
			eventType: event.PRApproved,
			want:      false,
		},
		{
			name:      "filter-out subtracts from explicit include too",
			include:   []string{event.PRMerged, event.PRApproved},
			exclude:   []string{event.PRApproved},
			eventType: event.PRApproved,
			want:      false,
		},
		{
			name:      "blank entries in filter are ignored",
			include:   []string{"", " ", event.PRMerged},
			eventType: event.PRMerged,
			want:      true,
		},
		{
			name:      "reserved type ci:failed is in default filter",
			eventType: "ci:failed",
			want:      true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFilter(tc.include, tc.exclude)
			got := f.matches(tc.eventType)
			if got != tc.want {
				t.Errorf("matches(%q) with include=%v exclude=%v = %v, want %v",
					tc.eventType, tc.include, tc.exclude, got, tc.want)
			}
		})
	}
}

// TestTruncateLine verifies the genuinely tricky parts of truncateLine: rune
// (not byte) slicing so multi-byte UTF-8 characters survive truncation, and
// whitespace collapsing via strings.Fields.
func TestTruncateLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than max passes through", "hello", 10, "hello"},
		{"collapses internal whitespace", "a\nb\tc  d", 20, "a b c d"},
		{"trims leading and trailing whitespace", "  hi  ", 20, "hi"},
		{"truncates ASCII with ellipsis", "abcdefghij", 5, "abcd…"},
		{"counts runes not bytes for length check", "héllo", 5, "héllo"},
		{"truncates multi-byte runes without splitting", "日本語テスト", 4, "日本語…"},
		{"max==1 returns one rune (no ellipsis room)", "日本語", 1, "日"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateLine(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("truncateLine(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// klausBinary lazily builds the real klaus binary and returns its path. All
// integration tests below run klaus as a subprocess so we exercise the full
// command surface including SIGTERM handling, line-buffered stdout, and
// fsnotify-driven streaming.
func klausBinary(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "klaus")
	// internal/cmd is two levels below the module root.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	modRoot := filepath.Dir(filepath.Dir(cwd))
	build := exec.Command("go", "build", "-o", bin, "./cmd/klaus")
	build.Dir = modRoot
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("go build klaus: %v", err)
	}
	return bin
}

type watchProc struct {
	cmd    *exec.Cmd
	stdout *bufio.Scanner
	stderr *bytes.Buffer
	done   chan error
}

func startWatch(t *testing.T, bin, sessionID, home string, args ...string) *watchProc {
	t.Helper()
	all := append([]string{"watch", "--session-id", sessionID}, args...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, all...)
	cmd.Env = append(os.Environ(), "HOME="+home)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting klaus watch: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &watchProc{cmd: cmd, stdout: scanner, stderr: &stderr, done: done}
}

func (p *watchProc) nextLine(t *testing.T, timeout time.Duration) string {
	t.Helper()
	type result struct {
		line string
		ok   bool
	}
	ch := make(chan result, 1)
	go func() {
		ok := p.stdout.Scan()
		ch <- result{line: p.stdout.Text(), ok: ok}
	}()
	select {
	case r := <-ch:
		if !r.ok {
			t.Fatalf("klaus watch stdout closed (stderr: %s)", p.stderr.String())
		}
		return r.line
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for line from klaus watch (stderr: %s)", p.stderr.String())
		return ""
	}
}

// stopSIGTERM sends SIGTERM and verifies the process exits cleanly.
func (p *watchProc) stopSIGTERM(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			// Exit triggered by signal handler is expected to be clean (nil).
			// signal.NotifyContext + RunE returning nil yields exit code 0.
			t.Fatalf("klaus watch exited with error after SIGTERM: %v (stderr: %s)", err, p.stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatalf("klaus watch did not exit within 5s after SIGTERM (stderr: %s)", p.stderr.String())
	}
}

// emitter is a helper that appends events to events.jsonl using the same
// writer the rest of klaus uses.
type emitter struct {
	log *event.Log
}

func newEmitter(sessionDir string) *emitter {
	return &emitter{log: event.NewLog(sessionDir)}
}

func (e *emitter) emit(t *testing.T, runID, typ string, data map[string]interface{}) {
	t.Helper()
	if err := e.log.Emit(event.New(runID, typ, data)); err != nil {
		t.Fatalf("emit event: %v", err)
	}
}

func setupSessionDir(t *testing.T) (home, sessionID, sessionDir string) {
	t.Helper()
	home = t.TempDir()
	sessionID = "test-watch-session"
	sessionDir = filepath.Join(home, ".klaus", "sessions", sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	return
}

// touchEventsFile pre-creates an empty events.jsonl so 'klaus watch' moves
// past its waitForFile phase without racing with a concurrent first emit.
// Production users hit the same race only if the file is created mid-startup,
// which is harmless — the wait loop polls every 500ms and picks it up.
func touchEventsFile(t *testing.T, sessionDir string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(sessionDir, "events.jsonl"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("creating events.jsonl: %v", err)
	}
	f.Close()
}

func TestWatchStreamsNewEvents(t *testing.T) {
	bin := klausBinary(t)
	home, sessionID, sessionDir := setupSessionDir(t)
	em := newEmitter(sessionDir)

	// Pre-write one event that should NOT appear (default: tail mode).
	em.emit(t, "old-run", event.PRMerged, map[string]interface{}{"pr_number": "1"})

	p := startWatch(t, bin, sessionID, home)

	// Give the subprocess a moment to seek to EOF and arm fsnotify.
	time.Sleep(300 * time.Millisecond)

	em.emit(t, "85", event.PRMerged, map[string]interface{}{"pr_number": "85"})
	line := p.nextLine(t, 5*time.Second)
	if !strings.Contains(line, "pr:merged") {
		t.Errorf("expected pr:merged line, got %q", line)
	}
	if !strings.Contains(line, "PR #85 merged") {
		t.Errorf("expected formatted PR #85 merged summary, got %q", line)
	}

	em.emit(t, "85", event.AgentPRCreated, map[string]interface{}{
		"pr_number": "85",
		"pr_url":    "https://github.com/owner/repo/pull/85",
	})
	line = p.nextLine(t, 5*time.Second)
	if !strings.Contains(line, "agent:pr-created") {
		t.Errorf("expected agent:pr-created line, got %q", line)
	}
	if !strings.Contains(line, "https://github.com/owner/repo/pull/85") {
		t.Errorf("expected PR URL in line, got %q", line)
	}

	p.stopSIGTERM(t)
}

func TestWatchSinceStartReplays(t *testing.T) {
	bin := klausBinary(t)
	home, sessionID, sessionDir := setupSessionDir(t)
	em := newEmitter(sessionDir)

	em.emit(t, "1", event.PRMerged, map[string]interface{}{"pr_number": "1"})
	em.emit(t, "2", event.PRMerged, map[string]interface{}{"pr_number": "2"})

	p := startWatch(t, bin, sessionID, home, "--since-start")

	line1 := p.nextLine(t, 5*time.Second)
	if !strings.Contains(line1, "PR #1 merged") {
		t.Errorf("expected first replayed event for PR #1, got %q", line1)
	}
	line2 := p.nextLine(t, 5*time.Second)
	if !strings.Contains(line2, "PR #2 merged") {
		t.Errorf("expected second replayed event for PR #2, got %q", line2)
	}

	p.stopSIGTERM(t)
}

func TestWatchFilterFlag(t *testing.T) {
	bin := klausBinary(t)
	home, sessionID, sessionDir := setupSessionDir(t)
	em := newEmitter(sessionDir)

	touchEventsFile(t, sessionDir)
	// --filter narrows to only pr:merged
	p := startWatch(t, bin, sessionID, home, "--filter", event.PRMerged)
	time.Sleep(300 * time.Millisecond)

	// This one should be filtered out — default would include it.
	em.emit(t, "9", event.AgentPRCreated, map[string]interface{}{
		"pr_number": "9", "pr_url": "https://example.com/pull/9",
	})
	// This one passes the filter.
	em.emit(t, "9", event.PRMerged, map[string]interface{}{"pr_number": "9"})

	line := p.nextLine(t, 5*time.Second)
	if !strings.Contains(line, "pr:merged") {
		t.Errorf("expected pr:merged after filtering, got %q", line)
	}
	if strings.Contains(line, "agent:pr-created") {
		t.Errorf("did not expect agent:pr-created to leak past --filter, got %q", line)
	}

	p.stopSIGTERM(t)
}

func TestWatchFilterOutFlag(t *testing.T) {
	bin := klausBinary(t)
	home, sessionID, sessionDir := setupSessionDir(t)
	em := newEmitter(sessionDir)

	touchEventsFile(t, sessionDir)
	p := startWatch(t, bin, sessionID, home, "--filter-out", event.PRApproved)
	time.Sleep(300 * time.Millisecond)

	em.emit(t, "12", event.PRApproved, map[string]interface{}{"pr_number": "12"})
	em.emit(t, "12", event.PRMerged, map[string]interface{}{"pr_number": "12"})

	line := p.nextLine(t, 5*time.Second)
	if strings.Contains(line, "pr:approved") {
		t.Errorf("pr:approved should have been excluded, got %q", line)
	}
	if !strings.Contains(line, "pr:merged") {
		t.Errorf("expected pr:merged to pass through filter-out, got %q", line)
	}

	p.stopSIGTERM(t)
}

func TestWatchJSONPassthrough(t *testing.T) {
	bin := klausBinary(t)
	home, sessionID, sessionDir := setupSessionDir(t)
	em := newEmitter(sessionDir)

	touchEventsFile(t, sessionDir)
	p := startWatch(t, bin, sessionID, home, "--json")
	time.Sleep(300 * time.Millisecond)

	em.emit(t, "42", event.PRMerged, map[string]interface{}{
		"pr_number": "42",
		"pr_url":    "https://github.com/owner/repo/pull/42",
	})

	line := p.nextLine(t, 5*time.Second)
	var evt event.Event
	if err := json.Unmarshal([]byte(line), &evt); err != nil {
		t.Fatalf("expected valid JSON line, got %q: %v", line, err)
	}
	if evt.Type != event.PRMerged {
		t.Errorf("type = %q, want %q", evt.Type, event.PRMerged)
	}
	if evt.RunID != "42" {
		t.Errorf("run_id = %q, want %q", evt.RunID, "42")
	}
	if evt.Data["pr_number"] != "42" {
		t.Errorf("pr_number = %v, want 42", evt.Data["pr_number"])
	}

	// Verify byte-for-byte equivalence to the original line in the events file.
	// The emitter appends a single JSON object per line so reading the file
	// gives us the exact line klaus watch should have emitted.
	raw, err := os.ReadFile(filepath.Join(sessionDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("reading events.jsonl: %v", err)
	}
	srcLines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(srcLines) != 1 {
		t.Fatalf("expected 1 source line, got %d", len(srcLines))
	}
	if line != srcLines[0] {
		t.Errorf("watch --json output not byte-equivalent to source\n got:  %q\n want: %q", line, srcLines[0])
	}

	p.stopSIGTERM(t)
}

func TestWatchExitsOnSIGTERMCleanly(t *testing.T) {
	bin := klausBinary(t)
	home, sessionID, _ := setupSessionDir(t)
	// No emits — just confirm SIGTERM during idle wait causes clean exit.

	p := startWatch(t, bin, sessionID, home)
	time.Sleep(200 * time.Millisecond)
	p.stopSIGTERM(t)
}

func TestWatchRequiresSessionID(t *testing.T) {
	bin := klausBinary(t)
	cmd := exec.Command(bin, "watch")
	// Clear KLAUS_SESSION_ID from the env in case the test runner has one set.
	env := []string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "KLAUS_SESSION_ID=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected klaus watch to fail without KLAUS_SESSION_ID, succeeded")
	}
	if !strings.Contains(stderr.String(), "must be run inside a klaus session") {
		t.Errorf("expected helpful error message about missing session, got: %s", stderr.String())
	}
}

func TestWatchListTypes(t *testing.T) {
	bin := klausBinary(t)
	cmd := exec.Command(bin, "watch", "--list-types")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("klaus watch --list-types failed: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "Live event types") {
		t.Errorf("expected 'Live event types' heading, got: %s", out)
	}
	if !strings.Contains(out, "Reserved event types") {
		t.Errorf("expected 'Reserved event types' heading, got: %s", out)
	}
	if !strings.Contains(out, event.PRMerged) {
		t.Errorf("expected pr:merged in output, got: %s", out)
	}
	if !strings.Contains(out, "ci:failed") {
		t.Errorf("expected ci:failed (reserved) in output, got: %s", out)
	}
}

// Sanity check that multiple emissions arrive in order.
func TestWatchPreservesOrder(t *testing.T) {
	bin := klausBinary(t)
	home, sessionID, sessionDir := setupSessionDir(t)
	em := newEmitter(sessionDir)

	touchEventsFile(t, sessionDir)
	p := startWatch(t, bin, sessionID, home, "--filter", event.PRMerged)
	time.Sleep(300 * time.Millisecond)

	const n = 5
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			em.emit(t, "ord", event.PRMerged, map[string]interface{}{"pr_number": strconv.Itoa(i)})
			time.Sleep(50 * time.Millisecond)
		}
	}()

	for i := 0; i < n; i++ {
		line := p.nextLine(t, 5*time.Second)
		want := "PR #" + strconv.Itoa(i) + " merged"
		if !strings.Contains(line, want) {
			t.Errorf("line %d: expected %q in %q", i, want, line)
		}
	}
	wg.Wait()
	p.stopSIGTERM(t)
}

//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// TestDashboardScrollsLongRunList runs the real dashboard in a small tmux pane
// over a long session: 4 running agents and 40 merged PRs, 30 of them merged
// over an hour ago. It drives the keys through tmux and reads the screen back.
func TestDashboardScrollsLongRunList(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	strPtr := func(s string) *string { return &s }
	now := time.Now()
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	cost := 1.0
	for i := 1; i <= 4; i++ {
		// Unfinalized, on a live pane: the dashboard sees these as running.
		pane := h.InitialPane
		h.SeedState(&run.State{
			ID: fmt.Sprintf("20261004-1200-a%03d", i), Type: "launch", Prompt: fmt.Sprintf("active %02d", i),
			TargetRepo: strPtr("acme/widget"), TmuxPane: &pane, Worktree: "/wt",
			CreatedAt: ago(time.Duration(i) * time.Minute),
		})
	}
	for i := 1; i <= 40; i++ {
		merged := time.Duration(i) * 5 * time.Minute
		if i > 10 {
			merged += 2 * time.Hour
		}
		h.SeedState(&run.State{
			ID: fmt.Sprintf("20261004-1200-m%03d", i), Type: "launch", Prompt: fmt.Sprintf("merged %02d", i),
			TargetRepo: strPtr("acme/widget"), PRURL: strPtr(fmt.Sprintf("https://github.com/acme/widget/pull/%d", 100+i)),
			CostUSD: &cost, CreatedAt: ago(merged + 30*time.Minute), MergedAt: strPtr(ago(merged)),
		})
	}

	pane := strings.TrimSpace(h.tmux("new-session", "-d", "-s", "dash", "-x", "70", "-y", "14", "-c", h.RepoDir,
		"-P", "-F", "#{pane_id}", "env KLAUS_SESSION_ID="+h.SessionID+" klaus dashboard"))

	screen := func(want ...string) string {
		t.Helper()
		var last string
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			last = h.tmux("capture-pane", "-p", "-t", pane)
			ok := true
			for _, w := range want {
				ok = ok && strings.Contains(last, w)
			}
			if ok {
				return last
			}
		}
		t.Fatalf("dashboard screen never showed %q; last screen:\n%s", want, last)
		return ""
	}
	selected := func(s string) string {
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "> ") {
				return line
			}
		}
		return ""
	}

	// 14 lines: header, source, summary, blank, 8 rows, blank, footer. The
	// running agents come first; the 30 old merged PRs are hidden.
	s := screen("1–8 of 14", "30 hidden")
	if sel := selected(s); !strings.Contains(sel, "active 01") {
		t.Errorf("first row should be selected, got %q:\n%s", sel, s)
	}

	h.tmux("send-keys", "-t", pane, "G")
	s = screen("7–14 of 14")
	if sel := selected(s); !strings.Contains(sel, "#110") {
		t.Errorf("G should select the last row (#110), got %q:\n%s", sel, s)
	}

	h.tmux("send-keys", "-t", pane, "h")
	s = screen("of 44", "all shown")
	if sel := selected(s); !strings.Contains(sel, "#110") {
		t.Errorf("show all should keep #110 selected and in view, got %q:\n%s", sel, s)
	}

	// Shrink the pane: the selection stays in view and nothing wraps.
	h.tmux("resize-window", "-t", "dash", "-x", "40", "-y", "8")
	s = screen("of 44")
	if lines := strings.Split(strings.TrimRight(s, "\n"), "\n"); len(lines) > 8 {
		t.Errorf("screen has %d lines in an 8-line pane:\n%s", len(lines), s)
	}
	if sel := selected(s); !strings.Contains(sel, "#110") {
		t.Errorf("after resize #110 should still be selected and visible, got %q:\n%s", sel, s)
	}
}

// TestDashboardShowsRunningConsult runs a real consult beside the real
// dashboard: its job shows as RUNNING while the backend works and as
// SUCCEEDED once it answers, and tab then enter reads the answer.
func TestDashboardShowsRunningConsult(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	// The dashboard shows nothing until it has a run.
	h.SeedState(&run.State{ID: "20261017-1200-a001", Type: "launch", Prompt: "seeded run",
		CreatedAt: time.Now().Add(-time.Minute).Format(time.RFC3339)})
	release := filepath.Join(h.E2EDir, "release-codex")
	h.WriteStub("codex", "#!/bin/sh\ncat >/dev/null\nwhile [ ! -e "+release+" ]; do sleep 0.1; done\necho the e2e answer\n")

	pane := strings.TrimSpace(h.tmux("new-session", "-d", "-s", "dash", "-x", "120", "-y", "24", "-c", h.RepoDir,
		"-P", "-F", "#{pane_id}", "env KLAUS_SESSION_ID="+h.SessionID+" klaus dashboard"))
	screen := func(want ...string) string {
		t.Helper()
		var last string
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			last = h.tmux("capture-pane", "-p", "-t", pane)
			ok := true
			for _, w := range want {
				ok = ok && strings.Contains(last, w)
			}
			if ok {
				return last
			}
		}
		t.Fatalf("dashboard screen never showed %q; last screen:\n%s", want, last)
		return ""
	}
	screen("seeded run")

	consult := exec.Command(klausBinary, "consult", "--backend", "codex", "--timeout", "1m", "What is the e2e answer?")
	consult.Dir = h.RepoDir
	consult.Env = h.env()
	if err := consult.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = consult.Process.Kill(); _ = consult.Wait() })
	screen("jobs · 1 running", "consult:", "RUNNING", "What is the e2e answer?")

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := consult.Wait(); err != nil {
		t.Fatalf("consult: %v", err)
	}
	screen("jobs · 1 finished", "SUCCEEDED")

	h.tmux("send-keys", "-t", pane, "Tab")
	screen("succeeded | enter read")
	h.tmux("send-keys", "-t", pane, "Enter")
	screen("status:     succeeded", "the e2e answer", "esc back")
}

// TestDashboardScreenStaysWhole runs the real dashboard over a valley change
// in a pane its view fills, while the valley on PATH refuses `changes
// --checks`, as one from before the flag does. klaus notes the refusal in
// dashboard.log, not on the screen: a line written under the dashboard
// scrolls the pane, and the renderer, which redraws only the lines that
// changed, would leave the scrolled lines on screen for good. Output from
// outside klaus does the same; r redraws the whole screen.
func TestDashboardScreenStaysWhole(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub()
	sha := strings.Repeat("1", 40)
	ready := h.valleyRun("20261103-1644-0001", "prompt one", "nxvp", "Add a health check", 1, sha, time.Now().Add(-5*time.Minute))
	h.writeValleyChanges(map[string]any{
		"schema": "valley-changes/v1", "host": "classic-laddie",
		"repos": []any{map[string]any{
			"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil, "branches": []any{},
			"changes": []any{change(ready, "ready", 1, version(1, sha, "Add a health check", "not-landed"))},
		}},
	})
	hold := filepath.Join(h.E2EDir, "valley.checks-hold")
	for _, f := range []string{"valley.checks-refuse", "valley.checks-hold"} {
		if err := os.WriteFile(filepath.Join(h.E2EDir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 7 lines: header, source, summary, blank, the row, blank, footer.
	pane := strings.TrimSpace(h.tmux("new-session", "-d", "-s", "dash", "-x", "120", "-y", "7", "-c", h.RepoDir,
		"-P", "-F", "#{pane_id}", "env KLAUS_SESSION_ID="+h.SessionID+" klaus dashboard"))
	capture := func() []string {
		return strings.Split(strings.TrimRight(h.tmux("capture-pane", "-p", "-t", pane), "\n"), "\n")
	}
	// whole says what is wrong with the screen, if anything: each line in
	// its place, once.
	whole := func(lines []string) string {
		count := func(s string) int {
			n := 0
			for _, l := range lines {
				if strings.Contains(l, s) {
					n++
				}
			}
			return n
		}
		switch {
		case len(lines) != 7:
			return fmt.Sprintf("%d lines, want 7", len(lines))
		case !strings.Contains(lines[0], "klaus dashboard"):
			return "the header isn't the first line"
		case !strings.Contains(lines[1], "polling: 30s"):
			return "the source line isn't the second line"
		case !strings.Contains(lines[2], "1–1 of 1") || count("1–1 of 1") != 1:
			return "the summary isn't the third line, once"
		case !strings.HasPrefix(lines[4], "> widget  nxvp") || count("nxvp") != 1:
			return "the row isn't the fifth line, once"
		case !strings.Contains(lines[6], "q quit"):
			return "the footer isn't the last line"
		case count("does not take") > 0 || count("stray") > 0:
			return "output from under the dashboard is on screen"
		}
		return ""
	}
	waitWhole := func(after string) {
		t.Helper()
		var lines []string
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if lines = capture(); whole(lines) == "" {
				return
			}
		}
		t.Fatalf("%s: %s:\n%s", after, whole(lines), strings.Join(lines, "\n"))
	}

	// The valley refuses --checks once the dashboard is on screen, as one
	// that takes seconds to list does; the refusal comes before the listing
	// without --checks.
	waitWhole("at start")
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		plain := 0
		for _, c := range h.valleyCalls("changes") {
			if !strings.Contains(c, "--checks") {
				plain++
			}
		}
		if plain > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the dashboard never listed changes without --checks: %q", h.valleyCalls("changes"))
		}
	}
	waitWhole("after the valley refused --checks")
	if log, _ := os.ReadFile(filepath.Join(h.Home, ".klaus", "sessions", h.SessionID, "dashboard.log")); !strings.Contains(string(log), "does not take --checks") {
		t.Errorf("dashboard.log should note the refusal:\n%s", log)
	}

	// Something else writes to the pane: the view scrolls up under it.
	tty, err := os.OpenFile(strings.TrimSpace(h.tmux("display-message", "-p", "-t", pane, "#{pane_tty}")), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tty.WriteString("stray output\r\n")
	tty.Close()
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); whole(capture()) == ""; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stray output never showed; the test can't tell r redraws the screen")
		}
	}
	h.tmux("send-keys", "-t", pane, "r")
	waitWhole("after r")
}

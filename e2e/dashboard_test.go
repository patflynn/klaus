//go:build e2e

package e2e

import (
	"fmt"
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

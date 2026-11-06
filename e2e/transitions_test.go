//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
)

// TestDashboardReportsEachPRTransitionOnce runs the real dashboard over a
// session like the one of 2026-10-21: three PRs merged long ago, approved
// through klaus, and three open, approved PRs. GitHub (a gh stub) goes
// through outages in which `gh pr view --json state` fails as on a rate
// limit while the other queries still answer. Through reloads, outages and
// a restart, the old PRs report nothing, and each open PR reports its
// approval once and its merge once, including the one that merges while
// the dashboard is down.
func TestDashboardReportsEachPRTransitionOnce(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	prs := filepath.Join(h.E2EDir, "gh-prs")
	if err := os.MkdirAll(prs, 0o755); err != nil {
		t.Fatal(err)
	}
	setPR := func(n int, state string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(prs, fmt.Sprint(n)), []byte(state+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	down := filepath.Join(h.E2EDir, "gh-down")
	argv := filepath.Join(h.E2EDir, "gh.argv")
	stateCalls := filepath.Join(h.E2EDir, "gh.state-calls")
	h.WriteStub("gh", fmt.Sprintf(`#!/usr/bin/env bash
dir=%q
line=$(printf '%%s' "$*" | tr '\n' ' ')
echo "$line" >> "$dir/gh.argv"
if [[ "$line" == *graphql* ]]; then
  if [ -e "$dir/gh-down" ]; then
    echo "down" >> "$dir/gh.state-calls"
    echo "error connecting to api.github.com" >&2
    exit 1
  fi
  echo "up" >> "$dir/gh.state-calls"
  printf 'HTTP/2.0 200 OK\r\n\r\n{"data": {"repository": {'
  sep=""
  for f in "$dir"/gh-prs/*; do
    n=$(basename "$f")
    printf '%%s"pr%%s": {"state": "%%s", "mergeable": "MERGEABLE", "reviewDecision": "APPROVED", "headRefOid": "h%%s", "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "SUCCESS"}}}]}}' "$sep" "$n" "$(cat "$f")" "$n"
    sep=","
  done
  printf '}}}'
fi
exit 0
`, h.E2EDir))

	strPtr := func(s string) *string { return &s }
	approved := true
	cost := 1.0
	states := map[int]*run.State{}
	for i, n := range []int{101, 102, 103, 104, 105, 106} {
		st := &run.State{
			ID: fmt.Sprintf("20261021-0100-p%03d", n), Type: "launch", Prompt: fmt.Sprintf("pr %d", n),
			TargetRepo: strPtr("acme/widget"), PRURL: strPtr(fmt.Sprintf("https://github.com/acme/widget/pull/%d", n)),
			CostUSD: &cost, CreatedAt: time.Now().Add(-time.Duration(10-i) * time.Hour).Format(time.RFC3339),
		}
		if n <= 103 {
			st.Approved = &approved
			setPR(n, "MERGED")
		} else {
			setPR(n, "OPEN")
		}
		states[n] = st
		h.SeedState(st)
	}

	countLines := func(path, substr string) int {
		data, _ := os.ReadFile(path)
		n := 0
		for _, line := range strings.Split(string(data), "\n") {
			if line != "" && strings.Contains(line, substr) {
				n++
			}
		}
		return n
	}
	// pass waits for the dashboard to query GitHub, up or down, then for gh
	// to go quiet.
	pass := func(kind string, before int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for countLines(stateCalls, kind) < before+1 {
			if time.Now().After(deadline) {
				t.Fatalf("no %s pass: %d queries, want %d", kind, countLines(stateCalls, kind), before+1)
			}
			time.Sleep(100 * time.Millisecond)
		}
		for last := -1; ; time.Sleep(500 * time.Millisecond) {
			n := countLines(argv, "")
			if n == last {
				return
			}
			last = n
		}
	}
	// reload rewrites a run state; the dashboard reloads the runs and asks
	// GitHub about every PR.
	reload := func() { h.SeedState(states[106]) }
	outage := func() {
		t.Helper()
		if err := os.WriteFile(down, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		before := countLines(stateCalls, "down")
		reload()
		pass("down", before)
		if err := os.Remove(down); err != nil {
			t.Fatal(err)
		}
		before = countLines(stateCalls, "up")
		reload()
		pass("up", before)
	}

	prEvents := func() []string {
		f, err := os.Open(filepath.Join(h.Home, ".klaus", "sessions", h.SessionID, "events.jsonl"))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var out []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var e event.Event
			if json.Unmarshal(sc.Bytes(), &e) == nil && (e.Type == event.PRApproved || e.Type == event.PRMerged) {
				out = append(out, e.Type+" #"+e.RunID)
			}
		}
		return out
	}
	waitEvent := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			for _, e := range prEvents() {
				if e == want {
					return
				}
			}
		}
		t.Fatalf("no %s event; PR events: %q", want, prEvents())
	}
	startDashboard := func() {
		h.tmux("new-session", "-d", "-s", "dash", "-x", "120", "-y", "24", "-c", h.RepoDir,
			"env KLAUS_SESSION_ID="+h.SessionID+" klaus dashboard")
	}

	startDashboard()
	waitEvent("pr:approved #106")
	reload()
	pass("up", countLines(stateCalls, "up"))
	outage()
	outage()
	setPR(104, "MERGED")
	reload()
	// Events are written in the order the passes are handled, so once 104's
	// merge is in, every earlier pass has been handled.
	waitEvent("pr:merged #104")

	h.tmux("kill-session", "-t", "dash")
	setPR(105, "MERGED")
	startDashboard()
	waitEvent("pr:merged #105")
	reload()
	pass("up", countLines(stateCalls, "up"))
	outage()
	setPR(106, "MERGED")
	reload()
	waitEvent("pr:merged #106")

	want := map[string]bool{}
	for _, n := range []int{104, 105, 106} {
		want[fmt.Sprintf("pr:approved #%d", n)] = true
		want[fmt.Sprintf("pr:merged #%d", n)] = true
	}
	got := prEvents()
	seen := map[string]int{}
	for _, e := range got {
		seen[e]++
		if !want[e] || seen[e] > 1 {
			t.Errorf("unexpected or repeated event %s; PR events: %q", e, got)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("PR events = %q, want each of %v once", got, want)
	}
	if strings.Contains(h.GHArgv(), "pr merge") {
		t.Errorf("the dashboard tried to merge a PR:\n%s", h.GHArgv())
	}
}

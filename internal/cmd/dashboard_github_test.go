package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/patflynn/klaus/internal/config"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/run"
)

// fakeGHAPI puts a `gh` on PATH that logs each call, one line per call, and
// answers `gh api` from files in its directory: graphql.json for GraphQL
// (or the raw response in ratelimit, when present) and a zero behind_by for
// compares.
func fakeGHAPI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
d=$(dirname "$0")
line=$(printf '%s' "$*" | tr '\n' ' ')
printf '%s\n' "$line" >> "$d/calls.log"
case "$line" in
*graphql*)
  if [ -f "$d/ratelimit" ]; then cat "$d/ratelimit"; exit 1; fi
  printf 'HTTP/2.0 200 OK\r\n\r\n'; cat "$d/graphql.json" ;;
*compare*) printf 'HTTP/2.0 200 OK\r\n\r\n{"behind_by": 0}' ;;
*) echo "unexpected gh call: $line" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	return dir
}

// graphQLPRs is a PRSnapshots response for PRs 1..n of one repository; the
// PRs in merged read MERGED, the others open with passing CI.
func graphQLPRs(n int, merged ...int) string {
	var prs []string
	for i := 1; i <= n; i++ {
		state := "OPEN"
		for _, m := range merged {
			if m == i {
				state = "MERGED"
			}
		}
		prs = append(prs, fmt.Sprintf(`"pr%d": {"state": %q, "isDraft": false, "mergeable": "MERGEABLE", "reviewDecision": null,
"headRefOid": "head%d", "baseRef": {"target": {"oid": "base"}}, "labels": {"nodes": []},
"commits": {"nodes": [{"commit": {"committedDate": "2026-11-01T00:00:00Z", "statusCheckRollup": {"state": "SUCCESS"}}}]},
"author": {"__typename": "User", "login": "op"}, "reviews": {"nodes": []}, "comments": {"nodes": []}}`, i, state, i))
	}
	return `{"data": {"viewer": {"login": "op"}, "repository": {` + strings.Join(prs, ",") + `}}}`
}

// TestGHPollingCallBudget: the dashboard's GitHub polling costs one GraphQL
// call per repository however many PRs it tracks, compares a PR's head with
// its base only when they move, stops asking about a merged PR, and stops
// polling while GitHub rate-limits it.
func TestGHPollingCallBudget(t *testing.T) {
	dir := fakeGHAPI(t)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	calls := func() []string {
		t.Helper()
		data, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
		os.Remove(filepath.Join(dir, "calls.log"))
		return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(data)), " ", "_"))
	}
	count := func(lines []string, what string) int {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, what) {
				n++
			}
		}
		return n
	}
	url := func(n int) string { return "https://github.com/acme/widget/pull/" + strconv.Itoa(n) }
	tracked := func(n int) []*run.State {
		var states []*run.State
		for i := 1; i <= n; i++ {
			u := url(i)
			states = append(states, &run.State{ID: fmt.Sprintf("20261106-0000-%04d", i), Type: "launch", Prompt: fmt.Sprintf("pr %d", i),
				PRURL: &u, PR: strPtr(strconv.Itoa(i)), CostUSD: float64Ptr(1), CreatedAt: time.Now().Format(time.RFC3339)})
		}
		return states
	}

	cfg := config.Config{TrustedReviewers: []string{"gemini-code-assist[bot]"}}
	m := newDashboardModel(run.NewGitDirStore(t.TempDir()), cfg, gh.NewGHCLIClient(""))
	m.tmuxDeps = testDashboardTmuxDeps()
	poll := func() []string {
		t.Helper()
		m = send(m, m.fetchGHStatusCmd(m.states)())
		return calls()
	}

	// Three open PRs and a merged one: one query, and a compare for each
	// open PR, whose head and base are new.
	m.states = tracked(4)
	write("graphql.json", graphQLPRs(4, 4))
	first := poll()
	if g, c := count(first, "graphql"), count(first, "compare"); g != 1 || c != 3 {
		t.Fatalf("first poll: %d GraphQL calls and %d compares, want 1 and 3:\n%s", g, c, strings.Join(first, "\n"))
	}
	if st := m.ghStatus[url(4)]; st == nil || st.State != "MERGED" {
		t.Fatalf("PR 4 status = %+v, want MERGED", st)
	}

	// Nothing moved: one query, no compares, and the merged PR isn't in it.
	steady := poll()
	if len(steady) != 1 || count(steady, "graphql") != 1 {
		t.Fatalf("steady poll of 3 open PRs: calls %v, want one GraphQL query", steady)
	}
	if strings.Contains(steady[0], "pr4:") {
		t.Errorf("the merged PR is still asked about: %s", steady[0])
	}
	if st := m.ghStatus[url(4)]; st == nil || st.State != "MERGED" {
		t.Errorf("PR 4 status after it left the query = %+v, want MERGED", st)
	}

	// Twice the open PRs cost the same once their heads are compared.
	m.states = tracked(7)
	write("graphql.json", graphQLPRs(7, 4))
	poll()
	if steady := poll(); len(steady) != 1 {
		t.Errorf("steady poll of 6 open PRs: calls %v, want one GraphQL query", steady)
	}

	// A rate limit pauses polling until GitHub's reset, says so in the
	// header, and leaves the statuses as they were.
	reset := time.Now().Add(40 * time.Minute)
	write("ratelimit", fmt.Sprintf("HTTP/2.0 200 OK\r\nX-Ratelimit-Remaining: 0\r\nX-Ratelimit-Reset: %d\r\n\r\n"+
		`{"data": null, "errors": [{"type": "RATE_LIMITED", "message": "API rate limit exceeded"}]}`, reset.Unix()))
	if limited := poll(); len(limited) != 1 {
		t.Fatalf("rate-limited poll: calls %v, want the one refused query", limited)
	}
	if paused := poll(); len(paused) != 0 {
		t.Errorf("polls while rate-limited: calls %v, want none", paused)
	}
	m = send(m, tea.WindowSizeMsg{Width: 200, Height: 30})
	want := "GitHub rate-limited until " + time.Unix(reset.Unix(), 0).Format("15:04")
	if view := ansi.Strip(m.View()); !strings.Contains(view, want) {
		t.Errorf("header doesn't say %q:\n%s", want, view)
	}
	if st := m.ghStatus[url(1)]; st == nil || st.State != "OPEN" || st.CI != "passing" {
		t.Errorf("PR 1 status after the rate limit = %+v, want its last reading", st)
	}
}

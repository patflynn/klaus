//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// TestStatusRendersSeededRun is the warm-up scenario: it proves the harness
// can build + run the real binary, that HOME isolation routes state reads to
// the temp store, and that the rendered table reflects seeded state — all
// without touching tmux (the seeded run has no pane).
func TestStatusRendersSeededRun(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	budget := "5.00"
	h.SeedState(&run.State{
		ID:        "20260608-0101-deadbeef",
		Prompt:    "implement the frobnicator",
		Branch:    "agent/20260608-0101-deadbeef",
		Budget:    &budget,
		CreatedAt: "2026-06-08T01:01:00Z",
		// No TmuxPane and no PRURL: determineStatus -> "exited" with no tmux/gh calls.
	})

	res := h.RunKlaus("status")
	if res.ExitCode != 0 {
		t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}

	for _, want := range []string{"20260608-0101-deadbeef", "implement the frobnicator", "exited"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("status output missing %q\n--- stdout ---\n%s", want, res.Stdout)
		}
	}
}

// TestStatusKeepsAttentionAFailedSuccessorTookOn: a run that took on a
// salvaged run's work (took_over) and then failed carries its attention,
// so klaus status shows it needs-attention, and the salvaged run
// superseded, whether GitHub says its PR is merged, closed or out of
// reach. A run's PR that merged with no attention taken on shows merged.
func TestStatusKeepsAttentionAFailedSuccessorTookOn(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteStub("gh", `#!/usr/bin/env bash
case "$*" in
  *"--json state"*) ;;
  *) exit 0 ;;
esac
case "$*" in
  *pull/77|*pull/80) echo MERGED ;;
  *pull/78) echo CLOSED ;;
  *pull/79) echo 'GraphQL: Could not resolve to a PullRequest with the number of 79. (repository.pullRequest)' >&2; exit 1 ;;
esac
exit 0
`)
	cost, took := 0.5, int64(1000)
	pr := func(n int) *string { return strPtrE2E(fmt.Sprintf("https://github.com/acme/widget/pull/%d", n)) }
	want := map[string]string{}
	for i, num := range []int{77, 78, 79} {
		change := strings.Repeat(string(rune('k'+i)), 32)
		salvaged := h.seedSalvaged(fmt.Sprintf("20261021-2300-0000000%d", i), "agent/"+change[:4], strings.Repeat("a", 40), change)
		successor := fmt.Sprintf("20261021-2300-0000001%d", i)
		h.SeedState(&run.State{ID: successor, Type: "pr-fix", Prompt: "carry on", TargetRepo: strPtrE2E("acme/widget"),
			Branch: salvaged.Branch, ChangeID: change, PRURL: pr(num), TookOver: []string{salvaged.ID},
			FailureReason: strPtrE2E("error_during_execution"), CostUSD: &cost, DurationMS: &took,
			CreatedAt: time.Now().Format(time.RFC3339)})
		want[salvaged.ID], want[successor] = "superseded", "needs-attention"
	}
	h.SeedState(&run.State{ID: "20261021-2300-00000020", Type: "launch", Prompt: "an ordinary PR", TargetRepo: strPtrE2E("acme/widget"),
		Branch: "agent/plain", ChangeID: strings.Repeat("n", 32), PRURL: pr(80), CostUSD: &cost, DurationMS: &took,
		CreatedAt: time.Now().Format(time.RFC3339)})
	want["20261021-2300-00000020"] = "merged"

	res := h.RunKlaus("status")
	if res.ExitCode != 0 {
		t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	for id, status := range want {
		var line string
		for _, l := range strings.Split(res.Stdout, "\n") {
			if strings.HasPrefix(l, id+" ") {
				line = l
			}
		}
		if f := strings.Fields(line); len(f) < 2 || f[1] != status {
			t.Errorf("run %s should be %s: %q\n%s", id, status, line, res.Stdout)
		}
	}
}

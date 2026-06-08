//go:build e2e

package e2e

import (
	"strings"
	"testing"

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

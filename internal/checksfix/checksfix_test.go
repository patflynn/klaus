package checksfix

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/restack"
	"github.com/patflynn/klaus/internal/valley"
)

// TestPassCountsAttempts drives passes over a change whose checks keep
// failing on each new head. Each head gets one run; a run whose next head
// still fails counts as a failed attempt, and after 3 klaus stops, until
// checks pass and the count starts over. A change another principal owns
// gets nothing.
func TestPassCountsAttempts(t *testing.T) {
	dir := t.TempDir()
	l, err := LoadLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	var launched []Launch
	n := 0
	d := Deps{
		Principal: func(context.Context, string) string { return "klaus-env" },
		NewRunID:  func() (string, error) { n++; return fmt.Sprintf("run-%d", n), nil },
		Launch:    func(_ context.Context, la Launch) error { launched = append(launched, la); return nil },
		Running:   func(restack.Key, string) bool { return false },
		Now:       time.Now,
	}
	repos := []restack.Repo{{Name: "widget", Dir: dir}}
	listing := func(owner, head, state string) *valley.Changes {
		v := valley.Version{N: 1, Head: head, Branch: "agent/x", Landing: valley.NotLanded,
			Checks: &valley.Checks{State: state, Required: []valley.Check{{Name: "web-unit", State: state, Why: "boom"}}}}
		return &valley.Changes{Repos: []valley.Repo{{Repo: "widget", Changes: []valley.Change{{
			ChangeID: "kxyz", Handle: "kxyz", Owner: owner, State: valley.StateReady, Active: 1, Versions: []valley.Version{v}}}}}}
	}
	pass := func(owner, head, state, want string) {
		t.Helper()
		lines := Pass(context.Background(), d, l, repos, listing(owner, head, state), true)
		if got := strings.Join(lines, "\n"); !strings.Contains(got, want) {
			t.Errorf("head %s %s: pass said %q, want %q", head, state, got, want)
		}
	}

	pass("someone-else", "h0", valley.ChecksFailed, "")
	if len(launched) != 0 {
		t.Fatalf("another owner's change got a run: %+v", launched)
	}
	pass("klaus-env", "h1", valley.ChecksFailed, "fix run run-1 dispatched (attempt 1 of 3)")
	pass("klaus-env", "h1", valley.ChecksFailed, "fix run run-1 dispatched (attempt 1 of 3)")
	pass("klaus-env", "h2", valley.ChecksFailed, "fix run run-2 dispatched (attempt 2 of 3)")
	pass("klaus-env", "h3", valley.ChecksFailed, "fix run run-3 dispatched (attempt 3 of 3)")
	pass("klaus-env", "h4", valley.ChecksFailed, "stopped after 3 fix runs")
	pass("klaus-env", "h5", valley.ChecksFailed, "stopped after 3 fix runs")
	if len(launched) != 3 {
		t.Fatalf("launched %d runs, want 3", len(launched))
	}
	if la := launched[2]; la.Head != "h3" || la.Owner != "klaus-env" || la.Branch != "agent/x" || la.ChangeID != "kxyz" {
		t.Errorf("the third launch expects %+v", la)
	}

	pass("klaus-env", "h5", valley.ChecksPassed, "")
	pass("klaus-env", "h6", valley.ChecksFailed, "fix run run-4 dispatched (attempt 1 of 3)")

	// A restarted pass reads what the ledger recorded.
	if l, err = LoadLedger(dir); err != nil {
		t.Fatal(err)
	}
	pass("klaus-env", "h6", valley.ChecksFailed, "fix run run-4 dispatched (attempt 1 of 3)")
	if len(launched) != 4 {
		t.Errorf("launched %d runs, want 4", len(launched))
	}
}

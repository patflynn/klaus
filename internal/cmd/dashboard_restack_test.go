package cmd

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/restack"
	"github.com/patflynn/klaus/internal/run"
)

// TestChangeRowShowsRestackConflict: a change row says where a restack
// conflict on it stands, and one klaus stopped short of resolving is the
// operator's.
func TestChangeRowShowsRestackConflict(t *testing.T) {
	now := time.Now()
	results := map[string]integration.Result{}
	changes := map[integration.ChangeKey]integration.Result{}
	var states []*run.State
	pushed := func(id, prompt string, c byte) string {
		changeID, sha := strings.Repeat(string(c), 32), strings.Repeat(string(c-'k'+'1'), 40)
		s := &run.State{ID: id, Prompt: prompt, Type: "launch", ChangeID: changeID, TargetRepo: strPtr("o/r"),
			CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), CostUSD: float64Ptr(1),
			PushedBranch: strPtr("agent/" + prompt), PushedSHA: &sha}
		states = append(states, s)
		r := integration.Result{Verdict: integration.Ready, Change: &integration.Change{
			Repo: "r", ChangeID: changeID, Handle: changeID[:4], Version: 1, Head: sha, Active: true}}
		results[id], changes[integration.ChangeKey{Repo: "r", ChangeID: changeID}] = r, r
		return changeID
	}
	resolving := pushed("20261027-1200-0001", "resolving", 'k')
	stopped := pushed("20261027-1200-0002", "stopped", 'l')
	quiet := pushed("20261027-1200-0003", "quiet", 'm')

	m := dashboardModel{
		states: states, tmuxDeps: testDashboardTmuxDeps(), ghStatus: map[string]*prStatus{},
		integration: results, changes: changes, hideAfter: defaultHideFinishedAfter,
		restacks: map[integration.ChangeKey]restack.View{
			{Repo: "r", ChangeID: resolving}: {Key: restack.Key{Repo: "r", ChangeID: resolving}, State: restack.StateDispatched, RunID: "20261027-1300-0009", Attempts: 1},
			{Repo: "r", ChangeID: stopped}:   {Key: restack.Key{Repo: "r", ChangeID: stopped}, State: restack.StateStopped, Attempts: 3},
		},
	}
	m = send(m, tea.WindowSizeMsg{Width: 220, Height: 40})
	plain := ansi.Strip(m.View())

	for _, tc := range []struct{ prompt, want string }{
		{"resolving", "READY  restack conflict  resolution run 20261027-1300-0009 dispatched (attempt 1 of 3)"},
		{"stopped", "ATTN  restack conflict  stopped after 3 resolution runs"},
	} {
		if line := lineWith(plain, tc.prompt); !strings.Contains(line, tc.want) {
			t.Errorf("%s row should read %q: %q", tc.prompt, tc.want, line)
		}
	}
	if line := lineWith(plain, "quiet"); strings.Contains(line, "restack") {
		t.Errorf("a change with no conflict shows one: %q", line)
	}
	for _, r := range m.buildRows() {
		if r.changeID == stopped && (!r.attention || r.category != catYours) {
			t.Errorf("a conflict klaus stopped on should be the operator's: attention %v, %s", r.attention, categoryNames[r.category])
		}
		if r.changeID == quiet && r.restack != nil {
			t.Errorf("a change with no conflict has a restack view: %+v", r.restack)
		}
	}
}

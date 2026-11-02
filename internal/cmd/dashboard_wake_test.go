package cmd

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/patflynn/klaus/internal/config"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
)

// countingChecker counts the integration checks run.
type countingChecker struct{ n int }

func (c *countingChecker) Check(context.Context, []*run.State) integration.Report {
	c.n++
	return integration.Report{}
}

// TestValleyWakesCoalesce drives the dashboard's update loop with wakes,
// timer ticks and check results: wakes during a check ask for one check
// after it, not one each, and a tick armed before a woken check is dropped,
// so the checks never overlap and the timer never doubles.
func TestValleyWakesCoalesce(t *testing.T) {
	checker := &countingChecker{}
	m := newDashboardModel(run.NewGitDirStore(t.TempDir()), config.Config{}, gh.NewGHCLIClient(""))
	m.checker, m.restackOn = checker, false
	send := func(msg tea.Msg) tea.Cmd {
		t.Helper()
		next, cmd := m.Update(msg)
		m = next.(dashboardModel)
		return cmd
	}
	// finish runs a started check and delivers its result.
	finish := func(cmd tea.Cmd) tea.Cmd {
		t.Helper()
		msg, ok := cmd().(integrationMsg)
		if !ok {
			t.Fatalf("the check returned %T", msg)
		}
		return send(msg)
	}

	send(statesLoadedMsg{}) // starts the loop's first check, left unrun
	if !m.checking {
		t.Fatal("the first load should start a check")
	}
	// A burst of records while the first check runs: one wake held for
	// wakeSettle, then one more check asked for.
	send(valleyWakeMsg{})
	send(valleyWakeMsg{})
	if !m.wakeHeld {
		t.Fatal("a wake should wait for the rest of its burst")
	}
	send(valleyWakeCheckMsg{})
	send(valleyWakeCheckMsg{})
	again := send(integrationMsg{})
	if checker.n != 0 || again == nil || !m.checking || m.checkAgain {
		t.Fatalf("the check's end should start the one check the wakes asked for (checking=%v again=%v)", m.checking, m.checkAgain)
	}
	if tick := finish(again); tick == nil || m.checking {
		t.Fatal("a check with no wake pending should arm the timer")
	}
	stale := m.checkSeq

	// A wake while idle checks at once; the tick armed before it is
	// dropped, and the one the woken check arms counts.
	send(valleyWakeMsg{})
	woken := send(valleyWakeCheckMsg{})
	if woken == nil || !m.checking {
		t.Fatal("a wake while no check runs should start one")
	}
	if cmd := send(integrationTickMsg{seq: stale}); cmd != nil {
		t.Fatal("the timer should not start a check beside a woken one")
	}
	finish(woken)
	if cmd := send(integrationTickMsg{seq: stale}); cmd != nil {
		t.Fatal("a tick armed before a woken check should be dropped")
	}
	tick := send(integrationTickMsg{seq: m.checkSeq})
	if tick == nil || !m.checking {
		t.Fatal("the latest tick should start a check")
	}
	finish(tick)
	if checker.n != 3 {
		t.Errorf("ran %d checks, want 3: the woken two and the tick's", checker.n)
	}
}

// TestRemoteHost: projects on one valley host share a follow, whatever
// form their origins take.
func TestRemoteHost(t *testing.T) {
	for remote, want := range map[string]string{
		"git@classic-laddie:widget.git":             "classic-laddie",
		"classic-laddie:cosmo.git":                  "classic-laddie",
		"ssh://git@classic-laddie:2222/qinling.git": "classic-laddie",
		"https://valley.example/the-valley":         "valley.example",
		"/tmp/sandbox/origin":                       "/tmp/sandbox/origin",
	} {
		if got := remoteHost(remote); got != want {
			t.Errorf("remoteHost(%q) = %q, want %q", remote, got, want)
		}
	}
}

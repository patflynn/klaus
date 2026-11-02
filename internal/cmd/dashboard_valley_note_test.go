package cmd

import (
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/config"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/run"
)

// TestDashboardNotesFailedValleyCheck feeds the update loop a failed valley
// check and then a good one: the note shows while the latest check failed
// and clears on the next success.
func TestDashboardNotesFailedValleyCheck(t *testing.T) {
	m := newDashboardModel(run.NewGitDirStore(t.TempDir()), config.Config{}, gh.NewGHCLIClient(""))
	m.width, m.height = 120, 30
	m.states = []*run.State{}
	update := func(msg integrationMsg) {
		next, _ := m.Update(msg)
		m = next.(dashboardModel)
	}
	const note = "valley unreachable: timed out after 3m0s; showing git's view"

	update(integrationMsg{valleyErr: "timed out after 3m0s"})
	if v := m.View(); !strings.Contains(v, note) {
		t.Errorf("view lacks %q after a failed check:\n%s", note, v)
	}
	update(integrationMsg{valleyErr: "timed out after 3m0s"})
	if v := m.View(); strings.Count(v, "valley unreachable") != 1 {
		t.Errorf("a second failure should keep one note:\n%s", v)
	}
	update(integrationMsg{})
	if v := m.View(); strings.Contains(v, "valley unreachable") {
		t.Errorf("the note should clear after a successful check:\n%s", v)
	}
}

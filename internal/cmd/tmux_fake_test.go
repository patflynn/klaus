package cmd

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/patflynn/klaus/internal/tmux"
)

// fakeTmux is a complete in-memory tmux.Client for unit tests. It deliberately
// implements every method rather than embedding the exec-backed client: an
// embedded real client would make any method the fake forgets to override talk
// to the live tmux server, which is how `go test` inside an agent worktree once
// killed the coordinator's agents session (issue #295). Every method here is
// pure bookkeeping.
type fakeTmux struct {
	mu sync.Mutex

	// panes maps a session name to the pane IDs it contains. A session that
	// is absent from the map does not exist.
	panes map[string][]string
	// runningPanes holds panes whose command is still running; every other
	// known pane reports as idle.
	runningPanes map[string]bool
	// existingPanes holds panes that exist on the server. Panes listed in a
	// session or marked running count as existing too.
	existingPanes map[string]bool

	killedPanes    []string
	killedSessions []string
	splitCommands  []string
	newWindows     []string
}

// Compile-time check that the fake really covers the whole interface.
var _ tmux.Client = (*fakeTmux)(nil)

func (f *fakeTmux) paneKnown(id string) bool {
	if f.existingPanes[id] || f.runningPanes[id] {
		return true
	}
	for _, ps := range f.panes {
		for _, p := range ps {
			if p == id {
				return true
			}
		}
	}
	return false
}

func (f *fakeTmux) InSession() bool { return true }

func (f *fakeTmux) SplitWindow(_ context.Context, _, _, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.splitCommands = append(f.splitCommands, command)
	return fmt.Sprintf("%%split-%d", len(f.splitCommands)), nil
}

func (f *fakeTmux) SplitWindowSized(ctx context.Context, target, dir, command, _, _ string) (string, error) {
	return f.SplitWindow(ctx, target, dir, command)
}

func (f *fakeTmux) SessionExists(_ context.Context, session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.panes[session]
	return ok
}

func (f *fakeTmux) NewDetachedWindow(_ context.Context, session, _, _, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newWindows = append(f.newWindows, command)
	id := fmt.Sprintf("%%win-%d", len(f.newWindows))
	if f.panes == nil {
		f.panes = map[string][]string{}
	}
	f.panes[session] = append(f.panes[session], id)
	return id, nil
}

func (f *fakeTmux) SessionPanes(_ context.Context, session string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	panes, ok := f.panes[session]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", session)
	}
	return append([]string(nil), panes...), nil
}

func (f *fakeTmux) KillSession(_ context.Context, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killedSessions = append(f.killedSessions, session)
	delete(f.panes, session)
	return nil
}

func (f *fakeTmux) SetPaneTitle(context.Context, string, string) error            { return nil }
func (f *fakeTmux) LockPaneTitle(context.Context, string) error                   { return nil }
func (f *fakeTmux) RebalanceLayout(context.Context, string) error                 { return nil }
func (f *fakeTmux) SwapPane(context.Context, string, string) error                { return nil }
func (f *fakeTmux) SetWindowOption(context.Context, string, string, string) error { return nil }
func (f *fakeTmux) RenameWindow(context.Context, string, string) error            { return nil }
func (f *fakeTmux) SendKeys(context.Context, string, string) error                { return nil }
func (f *fakeTmux) SelectPane(context.Context, string) error                      { return nil }

func (f *fakeTmux) ListWindowPanes(_ context.Context, target string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, panes := range f.panes {
		for _, p := range panes {
			if p == target {
				return append([]string(nil), panes...), nil
			}
		}
	}
	return nil, fmt.Errorf("pane not found: %s", target)
}

func (f *fakeTmux) PaneExists(_ context.Context, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paneKnown(id)
}

func (f *fakeTmux) PaneIsDead(_ context.Context, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paneKnown(id) && !f.runningPanes[id]
}

func (f *fakeTmux) PaneIsIdle(ctx context.Context, id string) bool {
	return f.PaneIsDead(ctx, id)
}

func (f *fakeTmux) CapturePane(context.Context, string, int) (string, error) { return "", nil }

func (f *fakeTmux) KillPane(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killedPanes = append(f.killedPanes, id)
	return nil
}

// testSessionID is the KLAUS_SESSION_ID unit tests pin themselves to. Agent
// panes export the coordinator's real KLAUS_SESSION_ID, so a test that leaves
// it alone would compute the live agents session name (issue #295).
const testSessionID = "test-session-xyz"

// isolatedTmux returns a fake tmux client and pins KLAUS_SESSION_ID so
// agentsSessionName() can never resolve to a session on the real server.
func isolatedTmux(t *testing.T) *fakeTmux {
	t.Helper()
	t.Setenv(sessionIDEnv, testSessionID)
	return &fakeTmux{}
}

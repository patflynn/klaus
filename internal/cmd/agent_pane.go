package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
)

// agentsSessionName is the detached tmux session holding this klaus session's
// agent panes. It is per-klaus-session so concurrent coordinators never share
// windows, and it is owned by the tmux server — so agents survive the
// coordinator exiting.
//
// Careful: agent panes export KLAUS_SESSION_ID (see tmuxSessionEnvPrefix), so
// every process an agent starts inside its worktree — including `go test` —
// inherits the coordinator's session id and resolves this to the live agents
// session. Tests must never let a real tmux client see this name; use a fake
// tmux client and t.Setenv(sessionIDEnv, ...) to pin it.
func agentsSessionName() string {
	id := os.Getenv(sessionIDEnv)
	if id == "" {
		return "klaus-agents"
	}
	// tmux treats "." and ":" as target separators.
	return "klaus-agents-" + strings.NewReplacer(".", "-", ":", "-").Replace(id)
}

// startAgentPane creates the tmux pane an agent runs in and applies the
// standard title/rename settings.
//
// In detached mode (the default) the pane is a window in the agents session,
// so the coordinator's window is left alone and splitFrom comes back empty. In
// "pane" mode the coordinator's window is split and splitFrom is the pane that
// was split, so the caller can re-pin its layout.
func startAgentPane(ctx context.Context, tc tmux.Client, mode, runID, dir, paneCmd, title string) (paneID, splitFrom string, err error) {
	if mode == config.AgentDisplayPane {
		splitFrom = os.Getenv("TMUX_PANE")
		paneID, err = tc.SplitWindow(ctx, splitFrom, dir, paneCmd)
	} else {
		paneID, err = tc.NewDetachedWindow(ctx, agentsSessionName(), runID, dir, paneCmd)
	}
	if err != nil {
		return "", "", fmt.Errorf("creating tmux pane: %w", err)
	}

	if err := tc.SetPaneTitle(ctx, paneID, title); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to set pane title: %v\n", err)
	}
	if err := tc.SetWindowOption(ctx, paneID, "automatic-rename", "off"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to disable automatic rename: %v\n", err)
	}
	if err := tc.LockPaneTitle(ctx, paneID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to lock pane title: %v\n", err)
	}

	if splitFrom != "" {
		if err := tc.RebalanceLayout(ctx, splitFrom); err != nil {
			return paneID, splitFrom, fmt.Errorf("rebalancing tmux layout: %w", err)
		}
	}
	return paneID, splitFrom, nil
}

// killEmptyAgentsSession drops the detached agents session once no remaining
// run holds a pane in it. tmux already destroys a session whose last window
// closes; this covers leftovers (e.g. a window whose command exited into a
// shell) so `klaus cleanup --all` leaves nothing behind.
//
// The store is only advisory — it can be stale, or belong to a different
// klaus session entirely — so the tmux server gets the final say: a session
// with any pane still running a command is never killed.
func killEmptyAgentsSession(ctx context.Context, store run.StateStore, tc tmux.Client) {
	name := agentsSessionName()
	panes, err := tc.SessionPanes(ctx, name)
	if err != nil {
		return // session does not exist — nothing to do
	}

	// The tmux server is the truth: a pane whose command is still running
	// belongs to a live agent, whatever the store says about it.
	for _, p := range panes {
		if tc.PaneExists(ctx, p) && !tc.PaneIsIdle(ctx, p) {
			fmt.Printf("  keeping agents tmux session %s (pane %s still running)\n", name, p)
			return
		}
	}

	states, err := store.List()
	if err != nil {
		return
	}
	held := make(map[string]bool, len(states))
	for _, s := range states {
		if s.TmuxPane != nil {
			held[*s.TmuxPane] = true
		}
	}
	for _, p := range panes {
		if held[p] {
			fmt.Printf("  keeping agents tmux session %s (pane %s still held by a run)\n", name, p)
			return
		}
	}

	if err := tc.KillSession(ctx, name); err == nil {
		fmt.Printf("  killed agents tmux session %s\n", name)
	}
}

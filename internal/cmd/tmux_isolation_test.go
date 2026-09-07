package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
)

// TestUnitTestsNeverUseRealTmux fails if a test in this package builds a tmux
// client that talks to the real server. Agent panes export KLAUS_SESSION_ID,
// so `go test` run by a klaus agent inherits the coordinator's session id; a
// real client here can reach — and kill — the live agents session (issue #295).
func TestUnitTestsNeverUseRealTmux(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("found no test files to scan")
	}

	banned := []struct{ frag, why string }{
		{"tmux.NewExecClient(", "constructs a client against the real tmux server"},
		{"tmux.ExecClient", "embedding ExecClient forwards un-overridden methods to the real tmux server"},
	}
	for _, f := range files {
		if f == "tmux_isolation_test.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range banned {
			if strings.Contains(string(src), b.frag) {
				t.Errorf("%s: %s — %s; use isolatedTmux(t) instead", f, b.frag, b.why)
			}
		}
	}
}

// TestCleanupAllNeverKillsAmbientAgentsSession is the regression test for
// issue #295: with a real-looking KLAUS_SESSION_ID in the environment and a
// store that knows nothing about the session's panes, `cleanup --all` must
// leave the live agents session alone.
func TestCleanupAllNeverKillsAmbientAgentsSession(t *testing.T) {
	t.Setenv(sessionIDEnv, "session-20260907-1241-c0da2e6d")
	ambient := agentsSessionName()

	tc := &fakeTmux{
		panes:        map[string][]string{ambient: {"%1", "%2"}},
		runningPanes: map[string]bool{"%1": true, "%2": true},
	}
	// A store holding runs that have nothing to do with those panes — the
	// shape every unit test's fake store has.
	store := newFakeStore(
		&run.State{ID: "run-1", Branch: "b1", CreatedAt: "2026-01-01T00:00:00Z"},
		&run.State{ID: "run-2", Branch: "b2", CreatedAt: "2026-01-01T00:01:00Z"},
	)
	deps := CleanupDeps{IsRunActive: func(*run.State) bool { return false }}

	captureStdout(t, func() {
		if err := cleanupAll(context.Background(), "", store, git.NewExecClient(), true, deps, tc); err != nil {
			t.Fatalf("cleanupAll() error: %v", err)
		}
	})

	for _, killed := range tc.killedSessions {
		if killed == ambient {
			t.Fatalf("cleanupAll killed the ambient agents session %q", ambient)
		}
	}
	if _, err := tc.SessionPanes(context.Background(), ambient); err != nil {
		t.Fatalf("ambient agents session %q no longer exists: %v", ambient, err)
	}
}

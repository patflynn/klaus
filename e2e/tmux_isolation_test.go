//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
)

// killServerWorker is the fake worker with, once released, what one ran on
// 2026-10-18 to kill what it took for its own test server: in a klaus pane
// $TMUX overrode TMUX_TMPDIR, and that killed the shared server, with the
// operator's session and every agent in it. It records the tmux environment
// it got and what tmux said, then asks klaus for status, as a worker's tools
// do, before printing its canned stream. tmux skips a TMUX_TMPDIR that doesn't
// exist for /tmp, where a developer's own server may be, so the worker only
// runs tmux where every TMUX_TMPDIR it uses exists.
func (h *Harness) killServerWorker() string {
	return strings.Replace(h.claudeStubScript(), `cat "$dir/claude.output.jsonl"`,
		`printf '%s\n' "TMUX=${TMUX-unset}" "TMUX_PANE=${TMUX_PANE-unset}" "TMUX_TMPDIR=${TMUX_TMPDIR-unset}" "`+tmux.SocketEnv+`=${`+tmux.SocketEnv+`-unset}" > "$dir/worker.env"
mkdir -p -m 700 "$dir/x"
if [ -z "${TMUX-}" ] && [ ! -d "${TMUX_TMPDIR-}" ]; then
  echo "not running tmux: TMUX_TMPDIR ${TMUX_TMPDIR-} is missing, so tmux would use /tmp" > "$dir/worker.tmux"
else
  {
    tmux kill-server
    TMUX_TMPDIR="$dir/x" tmux kill-server
    tmux ls
  } > "$dir/worker.tmux" 2>&1
fi
klaus status > "$dir/worker.status" 2>&1
cat "$dir/claude.output.jsonl"`, 1)
}

// TestWorkerCannotKillTheSharedServer: a worker of each backend runs
// `tmux kill-server`, `TMUX_TMPDIR=<dir> tmux kill-server` and `tmux ls`. The
// harness's server stands in for the shared one, its first session for the
// operator's, and both survive. The worker saw no $TMUX or $TMUX_PANE and its
// run's own TMUX_TMPDIR, yet klaus status in it still found its live pane on
// the shared server, and the run finalized as ever: state written, pane closed.
func TestWorkerCannotKillTheSharedServer(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "agy"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.useWorker(kind, kind+"-session")
			h.WriteStub(kind, h.killServerWorker())

			st, _ := h.launchRun("--backend", kind, "test the systemd timer")
			h.WaitForClaudeStart(30 * time.Second)
			pane := *st.TmuxPane
			if !h.PaneExists(pane) {
				t.Fatalf("worker pane %s missing before the worker ran", pane)
			}
			read := func(name string) string {
				b, _ := os.ReadFile(filepath.Join(h.E2EDir, name))
				return string(b)
			}

			// Watch the server while the run ends: a worker that reaches it
			// kills its own pane too, and the run never finalizes.
			h.ReleaseClaude()
			var final *run.State
			for deadline := time.Now().Add(60 * time.Second); final == nil; time.Sleep(100 * time.Millisecond) {
				if !h.SessionExists("main") || !h.PaneExists(h.InitialPane) {
					t.Fatalf("the worker's tmux killed the shared server's operator session; tmux said:\n%s", read("worker.tmux"))
				}
				s, err := h.ReadState(st.ID)
				if err == nil && s.DurationMS != nil && s.TmuxPane == nil && s.Worktree == "" {
					final = s
				} else if time.Now().After(deadline) {
					t.Fatalf("run did not finalize within 60s (last: %+v, %v)", s, err)
				}
			}

			tmuxDir := filepath.Join(h.Home, ".klaus", "sessions", h.SessionID, "tmux", st.ID)
			env := read("worker.env")
			for _, want := range []string{"TMUX=unset", "TMUX_PANE=unset", "TMUX_TMPDIR=" + tmuxDir, tmux.SocketEnv + "=" + h.Sock} {
				if !strings.Contains(env, want+"\n") {
					t.Errorf("worker environment lacks %q:\n%s", want, env)
				}
			}
			if fi, err := os.Stat(tmuxDir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
				t.Errorf("worker's TMUX_TMPDIR %s should be a 0700 directory: %v %v", tmuxDir, fi, err)
			}

			if status := read("worker.status"); !regexp.MustCompile(`(?m)^` + st.ID + `\s+running\s`).MatchString(status) {
				t.Errorf("klaus status in the worker did not find its live pane on the shared server:\n%s", status)
			}

			if final.Backend != kind || final.FailureReason != nil {
				t.Errorf("run did not finalize normally: %+v", final)
			}
			if h.PaneExists(pane) {
				t.Errorf("finalize left worker pane %s open", pane)
			}
		})
	}
}

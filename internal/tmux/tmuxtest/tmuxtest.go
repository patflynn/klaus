// Package tmuxtest keeps a package's tests off every real tmux server.
package tmuxtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/patflynn/klaus/internal/tmux"
)

// Main runs a package's tests as launch runs a worker: without $TMUX,
// $TMUX_PANE or $KLAUS_TMUX_SOCKET, and with a TMUX_TMPDIR of their own. A
// klaus agent's `go test` inherits its environment, and a coordinator's
// carries $TMUX, so without this any code under test that reaches the real
// tmux client talks to the shared server (issue #295). A test that wants a
// server starts its own with -S. Use it as
//
//	func TestMain(m *testing.M) { os.Exit(tmuxtest.Main(m)) }
func Main(m *testing.M) int {
	dir, err := os.MkdirTemp("", "klaus-tmux-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tmuxtest:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	for _, k := range []string{"TMUX", "TMUX_PANE", tmux.SocketEnv} {
		os.Unsetenv(k)
	}
	os.Setenv("TMUX_TMPDIR", dir)

	code := m.Run()

	// A server something started at the private default goes too. The socket
	// is named outright, so this can't reach any other server.
	sock := filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	if _, err := os.Stat(sock); err == nil {
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
	}
	return code
}

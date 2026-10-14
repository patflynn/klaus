package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// restartQuiet is how long the keyboard must be quiet before the dashboard
// restarts onto a new klaus, so a restart never lands mid-interaction.
const restartQuiet = 10 * time.Second

// binCheck notices when klaus on PATH names a different file than the
// running dashboard's binary, as when self-update installs a new klaus into
// the nix profile, so the dashboard can restart onto it.
type binCheck struct {
	running string          // the running binary, symlinks resolved
	path    func() string   // the PATH to search
	failed  map[string]bool // files an exec failed for: never tried again
}

func newBinCheck() *binCheck {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return &binCheck{running: exe, path: func() string { return os.Getenv("PATH") }, failed: map[string]bool{}}
}

// candidate returns the file klaus on PATH resolves to, symlinks resolved,
// when that is not the running binary or a file an exec failed for.
func (b *binCheck) candidate() (string, bool) {
	if b == nil {
		return "", false
	}
	for _, dir := range filepath.SplitList(b.path()) {
		if !filepath.IsAbs(dir) {
			continue // as exec.LookPath, which refuses relative entries
		}
		p, err := exec.LookPath(filepath.Join(dir, "klaus"))
		if err != nil {
			continue
		}
		r, err := filepath.EvalSymlinks(p)
		if err != nil || r == b.running || b.failed[r] {
			return "", false
		}
		return r, true
	}
	return "", false
}

// binUpdateMsg reports a new klaus on PATH that runs.
type binUpdateMsg struct {
	path    string
	version string // as its --version reports it, e.g. 0.4.1-1a2b3c4
}

// restartWaitMsg re-checks whether the dashboard is idle enough to restart.
type restartWaitMsg struct{}

// checkBinCmd looks for a new klaus on PATH and, if it runs --version,
// reports it.
func checkBinCmd(b *binCheck) tea.Cmd {
	return func() tea.Msg {
		p, ok := b.candidate()
		if !ok {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var out bytes.Buffer
		c := exec.CommandContext(ctx, p, "--version")
		c.Stdout = &out
		if err := c.Run(); err != nil {
			return nil // not runnable (yet): the next check looks again
		}
		v, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
		return binUpdateMsg{path: p, version: strings.TrimPrefix(v, "klaus version ")}
	}
}

func restartWaitCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return restartWaitMsg{} })
}

// restartWhenIdle quits so the dashboard can exec the new klaus, unless a
// key was pressed in the last restartQuiet; then it waits and asks again.
func (m dashboardModel) restartWhenIdle(now time.Time) (dashboardModel, tea.Cmd) {
	if now.Sub(m.lastKeyAt) < restartQuiet {
		return m, restartWaitCmd()
	}
	m.restarting = true
	return m, tea.Quit
}

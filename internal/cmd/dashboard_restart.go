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
	// version runs a klaus with --version; nil runs it for real.
	version func(path string) (string, error)
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

// restartTargetMsg is what klaus on PATH names just before a restart: the
// new klaus to exec, or nil when there is none any more, as when a rollback
// put back the running binary.
type restartTargetMsg struct{ target *binUpdateMsg }

// probeBin returns the new klaus on PATH if it runs --version, else nil.
// The probe can take seconds, so PATH is resolved again after it: if it no
// longer names the probed file, as when a rollback ran meanwhile, there is
// nothing to restart onto, and the next check starts over.
func probeBin(b *binCheck) *binUpdateMsg {
	p, ok := b.candidate()
	if !ok {
		return nil
	}
	version := b.version
	if version == nil {
		version = runVersion
	}
	v, err := version(p)
	if err != nil {
		return nil // not runnable (yet): the next check looks again
	}
	if now, ok := b.candidate(); !ok || now != p {
		return nil
	}
	return &binUpdateMsg{path: p, version: v}
}

// runVersion runs a klaus with --version and returns its version.
func runVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	c := exec.CommandContext(ctx, path, "--version")
	c.Stdout = &out
	if err := c.Run(); err != nil {
		return "", err
	}
	v, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
	return strings.TrimPrefix(v, "klaus version "), nil
}

// checkBinCmd looks for a new klaus on PATH and, if it runs --version,
// reports it.
func checkBinCmd(b *binCheck) tea.Cmd {
	return func() tea.Msg {
		if next := probeBin(b); next != nil {
			return *next
		}
		return nil
	}
}

func restartWaitCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return restartWaitMsg{} })
}

// restartWhenIdle waits while a key was pressed in the last restartQuiet.
// Once the keyboard is quiet, it resolves klaus on PATH again, since the
// profile may have changed while it waited.
func (m dashboardModel) restartWhenIdle(now time.Time) (dashboardModel, tea.Cmd) {
	if now.Sub(m.lastKeyAt) < restartQuiet {
		return m, restartWaitCmd()
	}
	b := m.bin
	return m, func() tea.Msg { return restartTargetMsg{target: probeBin(b)} }
}

// restartOnto quits so the dashboard can exec target, the klaus PATH names
// now. With no target, the pending restart and its notice are dropped.
func (m dashboardModel) restartOnto(target *binUpdateMsg, now time.Time) (dashboardModel, tea.Cmd) {
	if target == nil {
		m.restart, m.binNotice, m.binNoticeErr = nil, "", false
		return m, nil
	}
	m.restart = target
	m.binNotice, m.binNoticeErr = "klaus updated to "+target.version+"; restarting", false
	if now.Sub(m.lastKeyAt) < restartQuiet {
		return m, restartWaitCmd() // a key came in meanwhile
	}
	m.restarting = true
	return m, tea.Quit
}

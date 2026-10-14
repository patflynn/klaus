package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// profileFixture is a PATH with a nix profile's bin ahead of the system
// klaus. Its klaus is a symlink into a store of klaus builds, each a script
// that answers --version.
type profileFixture struct {
	t       *testing.T
	root    string
	profile string // the profile's bin dir
	system  string // the system klaus
	path    string // the PATH: a relative entry, the profile, the system bin
}

func newProfileFixture(t *testing.T) *profileFixture {
	f := &profileFixture{t: t, root: t.TempDir()}
	f.profile = filepath.Join(f.root, "profile", "bin")
	if err := os.MkdirAll(f.profile, 0o755); err != nil {
		t.Fatal(err)
	}
	f.system = f.build("system", "0.4.0-0000000")
	sep := string(filepath.ListSeparator)
	f.path = "relative/bin" + sep + f.profile + sep + filepath.Dir(f.system)
	return f
}

// build makes a klaus build in the store and returns its bin/klaus.
func (f *profileFixture) build(name, version string) string {
	f.t.Helper()
	p := filepath.Join(f.root, "store", name, "bin", "klaus")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho 'klaus version "+version+"'\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// link points the profile's klaus at target, as a profile install does.
func (f *profileFixture) link(target string) {
	f.t.Helper()
	_ = os.Remove(filepath.Join(f.profile, "klaus"))
	if err := os.Symlink(target, filepath.Join(f.profile, "klaus")); err != nil {
		f.t.Fatal(err)
	}
}

func (f *profileFixture) check(running string) *binCheck {
	return &binCheck{running: running, path: func() string { return f.path }, failed: map[string]bool{}}
}

// TestBinCheckFindsChangedKlaus drives the check with an injected PATH and
// running executable.
func TestBinCheckFindsChangedKlaus(t *testing.T) {
	f := newProfileFixture(t)
	oldStore := f.build("old", "0.4.1-1111111")
	newStore := f.build("new", "0.4.1-2222222")
	b := f.check(oldStore)

	// Nothing in the profile yet: PATH finds the system klaus, which is not
	// what runs.
	if got, ok := b.candidate(); !ok || got != f.system {
		t.Errorf("empty profile: got %q, %v; want the system klaus", got, ok)
	}
	f.link(oldStore)
	if got, ok := b.candidate(); ok {
		t.Errorf("profile links the running klaus: got %q, want no change", got)
	}
	f.link(newStore)
	if got, ok := b.candidate(); !ok || got != newStore {
		t.Errorf("profile links a new klaus: got %q, %v; want %q", got, ok, newStore)
	}
	b.failed[newStore] = true
	if got, ok := b.candidate(); ok {
		t.Errorf("exec of %s failed before: got %q, want it left alone", newStore, got)
	}
}

// pendingRestart returns a dashboard that found newStore on PATH just after
// a key press, so its restart waits.
func pendingRestart(t *testing.T, b *binCheck, newStore, version string) dashboardModel {
	t.Helper()
	m := dashboardModel{bin: b}
	next, _ := m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(dashboardModel)
	next, cmd := m.update(binUpdateMsg{path: newStore, version: version})
	m = next.(dashboardModel)
	if m.binNotice != "klaus updated to "+version+"; restarting" {
		t.Errorf("notice = %q", m.binNotice)
	}
	if isQuit(cmd) {
		t.Fatal("restarted at once, without showing the notice")
	}
	next, cmd = m.update(restartWaitMsg{})
	m = next.(dashboardModel)
	if isQuit(cmd) || m.restarting {
		t.Fatal("restarted a moment after a key press")
	}
	return m
}

// quietRestart lets the keyboard go quiet and runs the restart's last check.
func quietRestart(t *testing.T, m dashboardModel) (dashboardModel, bool) {
	t.Helper()
	m.lastKeyAt = time.Now().Add(-restartQuiet)
	next, cmd := m.update(restartWaitMsg{})
	m = next.(dashboardModel)
	if cmd == nil {
		t.Fatal("a quiet keyboard did not re-check PATH")
	}
	msg := cmd()
	if _, ok := msg.(restartTargetMsg); !ok {
		t.Fatalf("a quiet keyboard sent %T, want the re-check of PATH", msg)
	}
	next, cmd = m.update(msg)
	return next.(dashboardModel), isQuit(cmd)
}

// TestDashboardRestartWaitsForQuietKeyboard: a new klaus restarts the
// dashboard only once no key has been pressed for restartQuiet.
func TestDashboardRestartWaitsForQuietKeyboard(t *testing.T) {
	f := newProfileFixture(t)
	running, newStore := f.build("old", "0.4.1-1111111"), f.build("new", "0.4.1-2222222")
	f.link(newStore)
	m := pendingRestart(t, f.check(running), newStore, "0.4.1-2222222")

	m, quit := quietRestart(t, m)
	if !quit || !m.restarting || m.restart == nil || m.restart.path != newStore {
		t.Fatalf("quiet keyboard: quit=%v restarting=%v restart=%+v; want a restart onto %s", quit, m.restarting, m.restart, newStore)
	}
}

// TestDashboardRestartRechecksPath: the profile changes while the restart
// waits for the keyboard. The dashboard execs what PATH names at the end,
// or drops the restart when that is the running klaus again.
func TestDashboardRestartRechecksPath(t *testing.T) {
	t.Run("a newer klaus", func(t *testing.T) {
		f := newProfileFixture(t)
		running, first, second := f.build("old", "0.4.1-1111111"), f.build("first", "0.4.1-2222222"), f.build("second", "0.4.1-3333333")
		f.link(first)
		m := pendingRestart(t, f.check(running), first, "0.4.1-2222222")

		f.link(second)
		m, quit := quietRestart(t, m)
		if !quit || !m.restarting || m.restart == nil || m.restart.path != second || m.restart.version != "0.4.1-3333333" {
			t.Fatalf("quit=%v restarting=%v restart=%+v; want a restart onto %s, 0.4.1-3333333", quit, m.restarting, m.restart, second)
		}
		if m.binNotice != "klaus updated to 0.4.1-3333333; restarting" {
			t.Errorf("notice = %q, want the version restarted onto", m.binNotice)
		}
	})
	t.Run("rolled back to the running klaus", func(t *testing.T) {
		f := newProfileFixture(t)
		running, newStore := f.build("old", "0.4.1-1111111"), f.build("new", "0.4.1-2222222")
		f.link(newStore)
		m := pendingRestart(t, f.check(running), newStore, "0.4.1-2222222")

		f.link(running)
		m, quit := quietRestart(t, m)
		if quit || m.restarting || m.restart != nil {
			t.Fatalf("quit=%v restarting=%v restart=%+v; want the restart dropped", quit, m.restarting, m.restart)
		}
		if m.binNotice != "" {
			t.Errorf("notice = %q, want it cleared", m.binNotice)
		}
	})
}

// TestDashboardRestartPathChangesDuringProbe: a rollback while --version
// runs on the new klaus means the dashboard execs nothing; the pending
// restart is dropped, and a periodic check reports nothing.
func TestDashboardRestartPathChangesDuringProbe(t *testing.T) {
	f := newProfileFixture(t)
	running, newStore := f.build("old", "0.4.1-1111111"), f.build("new", "0.4.1-2222222")
	b := f.check(running)
	rollBackDuringProbe := func(path string) (string, error) {
		f.link(running)
		return "0.4.1-2222222", nil
	}

	f.link(newStore)
	b.version = rollBackDuringProbe
	if msg := checkBinCmd(b)(); msg != nil {
		t.Errorf("periodic check reported %+v; PATH named the running klaus again after the probe", msg)
	}

	f.link(newStore)
	b.version = nil
	m := pendingRestart(t, b, newStore, "0.4.1-2222222")
	b.version = rollBackDuringProbe
	m, quit := quietRestart(t, m)
	if quit || m.restarting || m.restart != nil || m.binNotice != "" {
		t.Fatalf("quit=%v restarting=%v restart=%+v notice=%q; want the restart dropped", quit, m.restarting, m.restart, m.binNotice)
	}
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

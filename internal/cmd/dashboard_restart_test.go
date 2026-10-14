package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestBinCheckFindsChangedKlaus drives the check with an injected PATH and
// running executable: a profile dir ahead of the system dir, its klaus a
// symlink into the store as nix profiles have it.
func TestBinCheckFindsChangedKlaus(t *testing.T) {
	root := t.TempDir()
	exe := func(p string) string {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	system := exe(filepath.Join(root, "system", "bin", "klaus"))
	oldStore := exe(filepath.Join(root, "store", "old", "bin", "klaus"))
	newStore := exe(filepath.Join(root, "store", "new", "bin", "klaus"))
	profile := filepath.Join(root, "profile", "bin")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	link := func(target string) {
		_ = os.Remove(filepath.Join(profile, "klaus"))
		if err := os.Symlink(target, filepath.Join(profile, "klaus")); err != nil {
			t.Fatal(err)
		}
	}
	pathEnv := "relative/bin" + string(filepath.ListSeparator) + profile + string(filepath.ListSeparator) + filepath.Dir(system)
	b := &binCheck{running: oldStore, path: func() string { return pathEnv }, failed: map[string]bool{}}

	// Nothing in the profile yet: PATH finds the system klaus, which is not
	// what runs.
	if got, ok := b.candidate(); !ok || got != system {
		t.Errorf("empty profile: got %q, %v; want the system klaus", got, ok)
	}
	link(oldStore)
	if got, ok := b.candidate(); ok {
		t.Errorf("profile links the running klaus: got %q, want no change", got)
	}
	link(newStore)
	if got, ok := b.candidate(); !ok || got != newStore {
		t.Errorf("profile links a new klaus: got %q, %v; want %q", got, ok, newStore)
	}
	b.failed[newStore] = true
	if got, ok := b.candidate(); ok {
		t.Errorf("exec of %s failed before: got %q, want it left alone", newStore, got)
	}
}

// TestDashboardRestartWaitsForQuietKeyboard: a new klaus restarts the
// dashboard only once no key has been pressed for restartQuiet.
func TestDashboardRestartWaitsForQuietKeyboard(t *testing.T) {
	m := dashboardModel{}
	next, _ := m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(dashboardModel)

	next, cmd := m.update(binUpdateMsg{path: "/nix/store/new/bin/klaus", version: "0.4.1-1a2b3c4"})
	m = next.(dashboardModel)
	if m.binNotice != "klaus updated to 0.4.1-1a2b3c4; restarting" {
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

	m.lastKeyAt = time.Now().Add(-restartQuiet)
	next, cmd = m.update(restartWaitMsg{})
	m = next.(dashboardModel)
	if !isQuit(cmd) || !m.restarting || m.restart == nil || m.restart.path != "/nix/store/new/bin/klaus" {
		t.Fatalf("quiet keyboard: quit=%v restarting=%v restart=%+v; want a restart onto the new klaus", isQuit(cmd), m.restarting, m.restart)
	}
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

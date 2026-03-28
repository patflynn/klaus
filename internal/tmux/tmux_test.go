package tmux

import (
	"testing"
)

func TestBuildArgsSplitWindow(t *testing.T) {
	args := BuildArgs("split-window", "-t", "%0", "-v", "-d", "-P", "-F", "#{pane_id}", "-c", "/tmp/test", "echo hello")
	want := []string{"split-window", "-t", "%0", "-v", "-d", "-P", "-F", "#{pane_id}", "-c", "/tmp/test", "echo hello"}

	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d", len(args), len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestBuildArgsKillPane(t *testing.T) {
	args := BuildArgs("kill-pane", "-t", "%5")
	want := []string{"kill-pane", "-t", "%5"}

	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d", len(args), len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestBuildArgsCapturePane(t *testing.T) {
	args := BuildArgs("capture-pane", "-t", "%5", "-p", "-S", "-500")
	want := []string{"capture-pane", "-t", "%5", "-p", "-S", "-500"}

	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d", len(args), len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestRebalanceLayoutEmptyPane(t *testing.T) {
	err := RebalanceLayout("")
	if err == nil {
		t.Fatal("expected error for empty targetPane, got nil")
	}
	want := "targetPane cannot be empty"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestBuildArgsSetWindowOption(t *testing.T) {
	args := BuildArgs("set-option", "-w", "-t", "%0", "automatic-rename", "off")
	want := []string{"set-option", "-w", "-t", "%0", "automatic-rename", "off"}

	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d", len(args), len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestBuildArgsRenameWindow(t *testing.T) {
	args := BuildArgs("rename-window", "-t", "%0", "my-repo")
	want := []string{"rename-window", "-t", "%0", "my-repo"}

	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d", len(args), len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestBuildArgsSplitWindowSized(t *testing.T) {
	args := BuildArgs("split-window", "-t", "%0", "-v", "-d", "-l", "30%", "-P", "-F", "#{pane_id}", "-c", "/tmp/test", "klaus dashboard")
	want := []string{"split-window", "-t", "%0", "-v", "-d", "-l", "30%", "-P", "-F", "#{pane_id}", "-c", "/tmp/test", "klaus dashboard"}

	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d", len(args), len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestBuildArgsSplitWindowSizedHorizontal(t *testing.T) {
	args := BuildArgs("split-window", "-t", "%0", "-h", "-d", "-l", "50%", "-P", "-F", "#{pane_id}", "-c", "/tmp/test", "echo hello")
	want := []string{"split-window", "-t", "%0", "-h", "-d", "-l", "50%", "-P", "-F", "#{pane_id}", "-c", "/tmp/test", "echo hello"}

	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d", len(args), len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestBuildArgsLockPaneTitle(t *testing.T) {
	args := BuildArgs("set-option", "-p", "-t", "%3", "allow-rename", "off")
	want := []string{"set-option", "-p", "-t", "%3", "allow-rename", "off"}

	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d", len(args), len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestInSessionOutsideTmux(t *testing.T) {
	// When running tests outside tmux, TMUX env var is typically not set
	// This test documents the behavior — it may pass or fail depending on env
	// The important thing is it doesn't panic
	_ = InSession()
}

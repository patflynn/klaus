package tmux

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
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
	err := RebalanceLayout(context.Background(), "")
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

func TestBuildArgsSetPaneTitle(t *testing.T) {
	args := BuildArgs("select-pane", "-t", "%3", "-T", "#42 fix login page crash")
	want := []string{"select-pane", "-t", "%3", "-T", "#42 fix login page crash"}

	if !reflect.DeepEqual(args, want) {
		t.Errorf("BuildArgs() got = %q, want %q", args, want)
	}
}

func TestBuildArgsLockPaneTitle(t *testing.T) {
	args := BuildArgs("set-option", "-p", "-t", "%3", "allow-rename", "off")
	want := []string{"set-option", "-p", "-t", "%3", "allow-rename", "off"}

	if !reflect.DeepEqual(args, want) {
		t.Errorf("BuildArgs() got = %q, want %q", args, want)
	}
}

func TestBuildArgsSwapPane(t *testing.T) {
	args := BuildArgs("swap-pane", "-s", "%3", "-t", "%5")
	want := []string{"swap-pane", "-s", "%3", "-t", "%5"}

	if !reflect.DeepEqual(args, want) {
		t.Errorf("BuildArgs() got = %q, want %q", args, want)
	}
}

func TestBuildArgsListPanes(t *testing.T) {
	args := BuildArgs("list-panes", "-t", "%0", "-F", "#{pane_id}")
	want := []string{"list-panes", "-t", "%0", "-F", "#{pane_id}"}

	if !reflect.DeepEqual(args, want) {
		t.Errorf("BuildArgs() got = %q, want %q", args, want)
	}
}

func TestInSessionOutsideTmux(t *testing.T) {
	// When running tests outside tmux, TMUX env var is typically not set
	// This test documents the behavior — it may pass or fail depending on env
	// The important thing is it doesn't panic
	_ = InSession()
}

// Issue #308: a pane command over tmux's imsg limit fails with "command too
// long". The error must lead with that, not with kilobytes of echoed command.
// Runs the real tmux against a throwaway server; $TMUX aims runTmux at it.
func TestRunTmuxErrorLeadsWithStderr(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	sock := filepath.Join(t.TempDir(), "sock")
	ctl := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", append([]string{"-S", sock, "-f", "/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	ctl("new-session", "-d", "-s", "main", "sleep 300")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })
	t.Setenv("TMUX", sock+","+ctl("display-message", "-p", "#{pid}")+",0")

	_, err := NewDetachedWindow(context.Background(), "klaus-agents-test", "run", t.TempDir(), "true "+strings.Repeat("x", 20000))
	if err == nil {
		t.Fatal("tmux accepted a 20KB command; the pane size guard may be stale")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "new-session: tmux new-session: command too long (args: tmux new-session -d -s klaus-agents-test") {
		t.Fatalf("tmux's message is not first: %.300s", msg)
	}
	if !regexp.MustCompile(`… \[truncated \d+ bytes\]\)$`).MatchString(msg) || len(msg) > 400 {
		t.Fatalf("args not truncated (%d bytes): %.500s", len(msg), msg)
	}
}

func TestTruncateBytesKeepsRunesWhole(t *testing.T) {
	if got := truncateBytes("ab", 5); got != "ab" {
		t.Fatalf("short input changed: %q", got)
	}
	got := truncateBytes("aé"+strings.Repeat("z", 10), 2)
	if got != "a… [truncated 12 bytes]" {
		t.Fatalf("got %q", got)
	}
}

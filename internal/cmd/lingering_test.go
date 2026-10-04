package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestLingeringProcessesFindsWorkInsideDir(t *testing.T) {
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("no /proc on this platform")
	}
	worktree := t.TempDir()
	sub := filepath.Join(worktree, "nested")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// A sibling sharing the worktree's name as a prefix is not inside it.
	sibling := worktree + "-other"
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sibling) })

	startSleep := func(dir string) {
		c := exec.Command("sleep", "60")
		c.Dir = dir
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	}

	if got := lingeringProcesses(worktree); len(got) != 0 {
		t.Fatalf("idle worktree reported %v", got)
	}
	startSleep(sibling)
	if got := lingeringProcesses(worktree); len(got) != 0 {
		t.Fatalf("process in sibling dir reported %v", got)
	}
	startSleep(sub)
	if got := lingeringProcesses(worktree); !slices.Equal(got, []string{"sleep"}) {
		t.Fatalf("lingeringProcesses = %v, want [sleep]", got)
	}
}

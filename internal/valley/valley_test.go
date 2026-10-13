package valley

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTimeoutKillsValleysChildren runs a fake valley whose ready starts a
// long-running child and waits for it, as a valley does while ssh carries
// out the ready: when the timeout ends the ready, the child is gone too.
func TestTimeoutKillsValleysChildren(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	dir, bin := t.TempDir(), t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := "#!/bin/sh\nsleep 300 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "valley"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Ready(ctx, dir, "nxvp-id", "0123456789abcdef0123456789abcdef01234567", "agent/x")
	var verr *Error
	if !errors.As(err, &verr) || !strings.HasPrefix(verr.Reason, "timed out") {
		t.Fatalf("Ready = %v, want a timeout", err)
	}
	// The child held valley's stdout open: had it survived, Ready would have
	// waited out WaitDelay.
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Ready took %s after a 500ms timeout", d)
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); alive(pid); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("valley's child %d outlived the timeout", pid)
		}
	}
}

// alive reports whether process pid exists and isn't a zombie.
func alive(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the parenthesized command name.
	i := strings.LastIndexByte(string(stat), ')')
	return i < 0 || !strings.HasPrefix(string(stat[i+1:]), " Z")
}

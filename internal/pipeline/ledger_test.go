package pipeline

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The environment TestLedgerRaceWorker reads: the ledger path, and which
// worker it is.
const (
	ledgerRacePathEnv = "KLAUS_TEST_LEDGER_RACE_PATH"
	ledgerRaceIDEnv   = "KLAUS_TEST_LEDGER_RACE_ID"
	ledgerRacePRs     = 25
)

// TestLedgerRecordAcrossProcesses has several processes, each with its own
// Ledger on one file, as several dashboards on one session have, record the
// same PRs and PRs of their own at once. Each mark must be reported by
// exactly one of them, and none may be lost to another's write.
func TestLedgerRecordAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), LedgerFile)
	const workers = 6

	type worker struct {
		cmd   *exec.Cmd
		start io.WriteCloser
		out   *bufio.Scanner
	}
	ws := make([]worker, workers)
	for i := range ws {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLedgerRaceWorker$")
		cmd.Env = append(os.Environ(), ledgerRacePathEnv+"="+path, ledgerRaceIDEnv+"="+strconv.Itoa(i))
		cmd.Stderr = os.Stderr
		start, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill() }) //nolint:errcheck
		ws[i] = worker{cmd: cmd, start: start, out: bufio.NewScanner(stdout)}
	}

	// Release the workers together once every one has loaded the ledger, so
	// their records overlap.
	for i, w := range ws {
		for w.out.Scan() && w.out.Text() != "ready" {
		}
		if w.out.Err() != nil || w.out.Text() != "ready" {
			t.Fatalf("worker %d never got ready: %v", i, w.out.Err())
		}
	}
	for _, w := range ws {
		w.start.Close()
	}

	reported := make(map[string]int)
	for i, w := range ws {
		var output []string
		for w.out.Scan() {
			if key, ok := strings.CutPrefix(w.out.Text(), "reported "); ok {
				reported[key]++
			} else {
				output = append(output, w.out.Text())
			}
		}
		if err := w.cmd.Wait(); err != nil {
			t.Fatalf("worker %d: %v\n%s", i, err, strings.Join(output, "\n"))
		}
	}

	var keys []string
	for i := 0; i < ledgerRacePRs; i++ {
		keys = append(keys, ledgerRaceKey("shared", i))
		for w := 0; w < workers; w++ {
			keys = append(keys, ledgerRaceKey(strconv.Itoa(w), i))
		}
	}
	saved, err := LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if n := reported[key]; n != 1 {
			t.Errorf("%s reported approved %d times, want once", key, n)
		}
		if !saved.has(key, markApproved) {
			t.Errorf("%s is missing from the saved ledger", key)
		}
	}
	if len(reported) != len(keys) {
		t.Errorf("reported %d PRs, want %d", len(reported), len(keys))
	}
}

// TestLedgerRaceWorker is one of TestLedgerRecordAcrossProcesses's
// processes. It loads the ledger, says it's ready, waits for its stdin to
// close, then records PRs every worker shares and PRs of its own, printing
// each mark it reports.
func TestLedgerRaceWorker(t *testing.T) {
	path := os.Getenv(ledgerRacePathEnv)
	if path == "" {
		t.Skip("run by TestLedgerRecordAcrossProcesses")
	}
	id := os.Getenv(ledgerRaceIDEnv)
	l, err := LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	io.Copy(io.Discard, os.Stdin) //nolint:errcheck
	for i := 0; i < ledgerRacePRs; i++ {
		for _, key := range []string{ledgerRaceKey("shared", i), ledgerRaceKey(id, i)} {
			first, err := l.record(key, markApproved)
			if err != nil {
				t.Fatal(err)
			}
			if first {
				fmt.Println("reported", key)
			}
		}
	}
}

func ledgerRaceKey(owner string, i int) string {
	return fmt.Sprintf("https://github.com/acme/%s/pull/%d", owner, i)
}

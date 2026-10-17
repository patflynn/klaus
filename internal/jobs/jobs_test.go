package jobs

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// A running record reads as died once its process is gone, or once its PID
// names a process that started at another time (a reused PID).
func TestRunningRecordDiesWithItsProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("start times come from /proc")
	}
	s := NewStore(t.TempDir())
	live := &Job{ID: "consult-live", Kind: KindConsult, Backend: "codex"}
	if err := s.Start(live); err != nil {
		t.Fatal(err)
	}
	if live.PID != os.Getpid() || live.PIDStart == "" {
		t.Fatalf("start didn't record this process: %+v", live)
	}

	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	gone := &Job{ID: "consult-gone", Kind: KindConsult, Status: Running, StartedAt: time.Now(), PID: exited.Process.Pid, PIDStart: "1"}
	reused := &Job{ID: "consult-reused", Kind: KindConsult, Status: Running, StartedAt: time.Now(), PID: os.Getpid(), PIDStart: live.PIDStart + "0"}
	for _, j := range []*Job{gone, reused} {
		if err := s.Save(j); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]Status{"consult-live": Running, "consult-gone": Died, "consult-reused": Died} {
		j, err := s.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != want {
			t.Errorf("%s reads %q, want %q", id, j.Status, want)
		}
	}
}

// The command name in /proc/<pid>/stat may hold spaces and parentheses; the
// fields are counted after the last ")". A zombie has exited.
func TestParseStatSkipsTheCommandName(t *testing.T) {
	const tail = " 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 777 19 20"
	for stat, want := range map[string]string{
		"4242 (klaus) S" + tail:         "777",
		"4242 (a b) (c) ) S" + tail:     "777",
		"4242 (klaus) Z" + tail:         "",
		"4242 (klaus) S 1 2":            "",
		"garbage without a paren S 1 2": "",
	} {
		got, ok := parseStat(stat)
		if got != want || ok != (want != "") {
			t.Errorf("parseStat(%q) = %q, %v; want %q", stat, got, ok, want)
		}
	}
}

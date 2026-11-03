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

// TestUnreadyReadsValleysAnswer runs Unready against a fake valley: the
// draft line means the change became a draft, a change that wasn't ready is
// no error whether valley says so or refuses for it, and any other refusal
// or output is one.
func TestUnreadyReadsValleysAnswer(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fakeValley := func(stdout, stderr string, code int) {
		t.Helper()
		script := "#!/bin/sh\n[ \"$*\" = 'unready nxvp-id' ] || { echo \"bad args: $*\" >&2; exit 9; }\n" +
			"printf '" + stdout + "'\nprintf '" + stderr + "' >&2\nexit " + strconv.Itoa(code) + "\n"
		if err := os.WriteFile(filepath.Join(bin, "valley"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name           string
		stdout, stderr string
		code           int
		want           Unreadied
		wantErr        string
	}{
		{name: "draft", stdout: "fetching...\\ncosmo nxvp draft\\n", want: Unreadied{Repo: "cosmo", Handle: "nxvp", Draft: true}},
		{name: "already a draft, said plainly", stdout: "nxvp is already a draft\\n", want: Unreadied{Note: "nxvp is already a draft"}},
		{name: "draft already, refused (real valley wording)", stderr: "valley: tymm is a draft already; ready it to resume it\\n", code: 1,
			want: Unreadied{Note: "valley: tymm is a draft already; ready it to resume it"}},
		{name: "withdrawn, refused (real valley wording)", stderr: "valley: kkkk is withdrawn, and a withdrawn change is never made a draft; ready it to bring it back\\n", code: 1,
			wantErr: "valley unready: valley: kkkk is withdrawn, and a withdrawn change is never made a draft; ready it to bring it back"},
		{name: "not ready, refused", stderr: "valley: change nxvp is not ready\\n", code: 1,
			want: Unreadied{Note: "valley: change nxvp is not ready"}},
		{name: "no record, refused", stderr: "valley: no change record for nxvp-id\\n", code: 1,
			want: Unreadied{Note: "valley: no change record for nxvp-id"}},
		{name: "other refusal", stderr: "valley: refused: you do not own nxvp\\n", code: 1,
			wantErr: "valley unready: valley: refused: you do not own nxvp"},
		{name: "unexpected output", stdout: "cosmo nxvp v2 ready\\n", wantErr: "valley unready: unexpected output: cosmo nxvp v2 ready"},
		{name: "no output", wantErr: "valley unready: unexpected output: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeValley(tc.stdout, tc.stderr, tc.code)
			got, err := Unready(context.Background(), dir, "nxvp-id")
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("Unready = %+v, %v; want error %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("Unready = %+v, %v; want %+v", got, err, tc.want)
			}
		})
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

// A CLI that refuses --checks is listed again without it, and not asked with
// it again until the refusal is forgotten; a refusal of anything else, or
// another failure, is an error, never a listing without checks.
func TestListChecksFallsBackOnRefusal(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
echo "$@" >> ` + filepath.Join(dir, "argv") + `
for a in "$@"; do
  if [ "$a" = --checks ]; then
    if [ -f ` + filepath.Join(dir, "other") + ` ]; then echo "valley: cannot reach host" >&2; else echo "valley: changes does not take --checks" >&2; fi
    exit 1
  fi
done
echo '{"schema":"valley-changes/v1","host":"h","repos":[]}'
`
	if err := os.WriteFile(filepath.Join(bin, "valley"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	checksRefused.Lock()
	checksRefused.at, checksRefused.logged = time.Time{}, false
	checksRefused.Unlock()
	argv := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "argv"))
		return string(b)
	}

	for i := 0; i < 2; i++ {
		if _, err := List(context.Background(), dir); err != nil {
			t.Fatalf("List = %v, want the plain listing", err)
		}
	}
	if got, want := argv(), "changes --all --json --checks\nchanges --all --json\nchanges --all --json\n"; got != want {
		t.Errorf("valley ran:\n%s\nwant:\n%s", got, want)
	}

	// Once forgotten, the flag is tried again; another failure is an error.
	old := ChecksRefusalMemory
	ChecksRefusalMemory = 0
	defer func() { ChecksRefusalMemory = old }()
	if err := os.WriteFile(filepath.Join(dir, "other"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := List(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "cannot reach host") {
		t.Errorf("List = %v, want the valley's own failure", err)
	}
}

package valley

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFollowAllStreams runs Follow against a fake valley: the follow names
// no source, so one follow carries every stream; records whose type can move
// a change wake, others don't; and the next follow resumes from the cursor,
// one position per stream, that the last one ended with.
func TestFollowAllStreams(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	const cursor = "integrator=0123456789abcdef:375,report.ci=fedcba9876543210:12,worker=00112233445566ff:75710"
	script := `#!/bin/sh
echo "$@" >> ` + filepath.Join(dir, "argv") + `
cat <<'EOF'
{"kind":"header","format":"valley-telemetry/v1","op":"follow"}
{"kind":"record","stream":"worker","seq":75709,"type":"change.seen","data":{}}
{"kind":"record","stream":"integrator","seq":375,"type":"decision","data":{}}
{"kind":"record","stream":"report.ci","seq":12,"type":"command","data":{}}
not a line of the answer
{"kind":"record","stream":"worker","seq":75710,"type":"evidence.stored","data":{}}
{"kind":"heartbeat","cursor":"integrator=0123456789abcdef:374"}
{"kind":"record","stream":"worker","seq":75711,"type":"batch.end","data":{}}
{"kind":"end","cursor":"` + cursor + `","truncated":false}
EOF
`
	if err := os.WriteFile(filepath.Join(bin, "valley"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	wakes := 0
	got, err := Follow(context.Background(), dir, "", func() { wakes++ })
	if err != nil {
		t.Fatalf("Follow = %v", err)
	}
	if got != cursor {
		t.Errorf("Follow returned cursor %q, want the end line's %q", got, cursor)
	}
	if wakes != 3 {
		t.Errorf("Follow woke %d times, want 3: the decision, the evidence and the batch's end", wakes)
	}
	if _, err := Follow(context.Background(), dir, got, func() {}); err != nil {
		t.Fatalf("resumed Follow = %v", err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if want := "telemetry --json follow\ntelemetry --json follow --since " + cursor + "\n"; string(argv) != want {
		t.Errorf("valley ran:\n%s\nwant:\n%s", argv, want)
	}
}

// TestFollowRefusedIsAnError: a host that refuses the follow, as when the
// principal's follow slots are all taken, fails the follow with valley's
// reason and no cursor to resume from.
func TestFollowRefusedIsAnError(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
echo '{"kind":"error","error":"the principal already has 4 observe requests open; try again when one ends"}'
echo "valley: the host refused: the principal already has 4 observe requests open; try again when one ends" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "valley"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := Follow(context.Background(), t.TempDir(), "", func() { t.Error("a refused follow woke") })
	var e *Error
	if !errors.As(err, &e) || !strings.Contains(e.Reason, "observe requests open") {
		t.Fatalf("Follow = %q, %v; want valley's refusal", got, err)
	}
	if got != "" {
		t.Errorf("a refused follow returned cursor %q", got)
	}
}

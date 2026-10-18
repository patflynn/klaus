package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/consult"
)

func TestConsultThreadIntegration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(sessionIDEnv, "session-consult-test")
	t.Setenv("KLAUS_BACKEND", "claude")
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if output, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %s, %v", output, err)
		}
	}
	rev, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, ".klaus", "sessions", "session-consult-test", "consults", "design.log")
	t.Setenv("CONSULT_TEST_LOG", logPath)
	stub := filepath.Join(t.TempDir(), "backend")
	script := `#!/bin/sh
case "$(tail -n 1 "$CONSULT_TEST_LOG")" in
 *'"status":"pending"'*) ;;
 *) echo missing-pending >&2; exit 19 ;;
esac
printf '%s\n' "$@"
printf 'session id: fake-thread-id\n' >&2
cat
`
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	old := consult.LookupBinary
	t.Cleanup(func() { consult.LookupBinary = old })
	consult.LookupBinary = func(name string) (string, error) {
		if name != "codex" {
			return "", os.ErrNotExist
		}
		return stub, nil
	}
	run := func(args ...string) (string, error) {
		c := newConsultCmd()
		var output bytes.Buffer
		c.SetOut(&output)
		c.SetErr(&output)
		c.SetArgs(args)
		err := c.Execute()
		return output.String(), err
	}
	first, err := run("--backend", "codex", "--thread", "design", "--dir", dir, "--role", "critic", "first question")
	if err != nil || !strings.Contains(first, "first question") {
		t.Fatalf("first: %s, %v", first, err)
	}
	store := consult.NewStore(filepath.Join(home, ".klaus", "sessions", "session-consult-test"))
	saved, err := store.Load("design")
	if err != nil || saved.BackendSessionID != "fake-thread-id" || saved.Turns != 1 {
		t.Fatalf("saved: %+v, %v", saved, err)
	}
	second, err := run("--thread", "design", "second question")
	if err != nil || !strings.Contains(second, "resume\nfake-thread-id") {
		t.Fatalf("second: %s, %v", second, err)
	}
	saved, err = store.Load("design")
	if err != nil || saved.Turns != 2 {
		t.Fatalf("second save: %+v, %v", saved, err)
	}
	log, err := os.ReadFile(filepath.Join(store.Dir, "design.log"))
	if err != nil || !strings.Contains(string(log), "first question") || !strings.Contains(string(log), "second question") {
		t.Fatalf("log: %s, %v", log, err)
	}
	var turns []consult.Turn
	for _, line := range bytes.Split(bytes.TrimSpace(log), []byte("\n")) {
		var turn consult.Turn
		if err := json.Unmarshal(line, &turn); err != nil {
			t.Fatal(err)
		}
		turns = append(turns, turn)
	}
	if len(turns) != 2 || turns[0].Status != "ok" || turns[0].SessionIDSource != "stderr" || turns[0].RepoRevision != strings.TrimSpace(string(rev)) || turns[1].ResumeID != "fake-thread-id" || turns[1].SessionIDSource != "stderr" {
		t.Fatalf("turn metadata: %+v", turns)
	}
	list, err := run("--list")
	if err != nil || !strings.Contains(list, "design\tcodex\t2\t") {
		t.Fatalf("list: %s, %v", list, err)
	}
	if _, err := run("--thread", "design", "--backend", "agy", "question"); err == nil {
		t.Fatal("backend change accepted")
	}
	events, err := os.ReadFile(filepath.Join(filepath.Dir(store.Dir), "events.jsonl"))
	if err != nil || strings.Count(string(events), "consult:completed") != 2 {
		t.Fatalf("events: %s, %v", events, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.Dir), "runs")); !os.IsNotExist(err) {
		t.Fatal("consult created run state")
	}
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho backend-failed >&2\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	failed, err := run("--backend", "codex", "--dir", dir, "question")
	if err == nil || !strings.Contains(failed, "backend-failed") {
		t.Fatalf("failure: %s, %v", failed, err)
	}
	failed, err = run("--thread", "design", "failed question")
	if err == nil {
		t.Fatal("failed thread accepted")
	}
	log, _ = os.ReadFile(logPath)
	if !strings.Contains(string(log), `"status":"error: `) || !strings.Contains(string(log), `"turn":3`) {
		t.Fatalf("failed transcript: %s", log)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(logPath), "design.3.raw"))
	if err != nil || !strings.Contains(string(raw), "backend-failed") {
		t.Fatalf("raw failure: %s, %v", raw, err)
	}
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho answer-without-id\necho diagnostic-without-id >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = run("--backend", "codex", "--thread", "missing-id", "--dir", dir, "question")
	rawPath := filepath.Join(filepath.Dir(logPath), "missing-id.1.raw")
	if err == nil {
		t.Fatal("missing ID accepted")
	}
	for _, part := range []string{"codex", "stderr", "stdout-json", "text", rawPath} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("missing diagnostic %q: %v", part, err)
		}
	}
	raw, err = os.ReadFile(rawPath)
	if err != nil || !strings.Contains(string(raw), "answer-without-id") || !strings.Contains(string(raw), "diagnostic-without-id") {
		t.Fatalf("raw extraction failure: %s, %v", raw, err)
	}
	if _, err := run("--backend", "codex", "--thread", "missing-id", "--dir", dir, "retry question"); err == nil {
		t.Fatal("missing ID retry accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(logPath), "missing-id.2.raw")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
if [ "$1" = exec ]; then
 echo unfinished-output
 sleep 30
else
 echo fast-answer
fi
`), 0700); err != nil {
		t.Fatal(err)
	}
	consult.LookupBinary = func(name string) (string, error) { return stub, nil }
	t.Setenv("KLAUS_BACKEND", "agy")
	started := time.Now()
	output, err := run("--panel", "--dir", dir, "--timeout", "200ms", "question")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
		t.Fatalf("timeout: %s, %v", output, err)
	}
	if !strings.Contains(output, "## codex (default)\n\ntimed out") || !strings.Contains(output, "fast-answer") || strings.Contains(output, "unfinished-output") {
		t.Fatalf("panel: %s", output)
	}
	if _, err := run("--backend", "codex", "--timeout", "0", "question"); err == nil {
		t.Fatal("zero deadline accepted")
	}

}

// Headless agy can't open a path outside its workspace, so each --file
// reaches it whole on stdin, with the question, under a header naming its
// path. Runs the real binary against a stand-in agy that records its input;
// the attachments total more than the 128KB a single exec argument may hold.
func TestConsultAgyGetsAttachedFiles(t *testing.T) {
	bin := klausBinary(t)
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, record := t.TempDir(), t.TempDir()
	plan, review := filepath.Join(outside, "plan.md"), filepath.Join(outside, "review.md")
	planText := "# The plan\n" + strings.Repeat("a step of the plan\n", 150*1024/19)
	reviewText := "The review ends without a newline"
	for path, text := range map[string]string{plan: planText, review: reviewText} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	argvFile, stdinFile := filepath.Join(record, "argv"), filepath.Join(record, "stdin")
	e := newConsultEnv(t, bin, "")
	e.stub(t, "agy", fmt.Sprintf("printf '%%s\\n' \"$@\" > %s\ncat > %s\necho the answer\n", shellQuote(argvFile), shellQuote(stdinFile)))

	out, err := e.run(t, "--backend", "agy", "--dir", workspace, "--file", plan, "--file", review, "Does the review hold?")
	if err != nil || !strings.Contains(out, "the answer") {
		t.Fatalf("consult: %s, %v", out, err)
	}
	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("Inspect the repository at %q.", workspace),
		"\n\nDoes the review hold?\n\n2 files are attached below, each in full",
		"===== BEGIN ATTACHED FILE 1 of 2: " + plan + " =====\n" + planText + "===== END ATTACHED FILE 1 of 2: " + plan + " =====",
		"===== BEGIN ATTACHED FILE 2 of 2: " + review + " =====\n" + reviewText + "\n===== END ATTACHED FILE 2 of 2: " + review + " =====",
	} {
		if !strings.Contains(string(stdin), want) {
			t.Errorf("agy's stdin lacks %.200q", want)
		}
	}
	argv, _ := os.ReadFile(argvFile)
	if strings.Contains(string(argv), "a step of the plan") || strings.Contains(string(argv), reviewText) || strings.Contains(string(argv), "\n--print\n") {
		t.Errorf("attachments in agy's argv: %.300s", argv)
	}

	// Past the total, the consult is refused and agy never runs.
	extra := filepath.Join(outside, "extra.md")
	if err := os.WriteFile(extra, bytes.Repeat([]byte("x"), 60*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(stdinFile)
	out, err = e.run(t, "--backend", "agy", "--dir", workspace, "--file", plan, "--file", extra, "Does the review hold?")
	if err == nil || !strings.Contains(out, "--file attachments exceed the 200KB total: "+extra) {
		t.Fatalf("over the total: %s, %v", out, err)
	}
	if _, err := os.Stat(stdinFile); !os.IsNotExist(err) {
		t.Fatal("agy ran for a refused consult")
	}
}

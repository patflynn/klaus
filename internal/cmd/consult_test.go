package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/consult"
)

func TestConsultThreadIntegration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(sessionIDEnv, "session-consult-test")
	t.Setenv("KLAUS_BACKEND", "claude")
	dir := t.TempDir()
	stub := filepath.Join(t.TempDir(), "backend")
	script := `#!/bin/sh
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
}

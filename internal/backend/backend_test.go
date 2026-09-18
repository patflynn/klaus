package backend

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Exercise the shell boundary with quotes, newlines and substitutions. The CLI
// stub receives the exact argv and must not execute any prompt content.
func TestWorkerShellBoundary(t *testing.T) {
	for _, k := range []Kind{Claude, Codex, Agy} {
		t.Run(string(k), func(t *testing.T) {
			dir := t.TempDir()
			stub := filepath.Join(dir, string(k))
			if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			o := Options{SystemPrompt: "instructions\nwith 'quotes'", Prompt: "do not execute $(exit 8) `exit 9`\n--help", Model: "model'quoted", Effort: "high", Budget: "5", RunID: "run"}
			args := k.Worker(o)
			args[0] = stub
			out, err := exec.Command("sh", "-c", ShellCommand(args)).Output()
			if err != nil {
				t.Fatal(err)
			}
			actual := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
			if !reflect.DeepEqual(actual, args[1:]) {
				t.Fatalf("argv changed across shell: %#v != %#v", actual, args[1:])
			}
			if k != Claude && strings.Contains(string(out), "--max-budget-usd") {
				t.Fatal("unsupported budget flag")
			}
			if k == Codex && !strings.Contains(string(out), "developer_instructions=") {
				t.Fatal("missing developer instructions")
			}
		})
	}
}

func TestAgyWorkspaceResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "cache")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"/work/one": "session-one", "/work/two": "session-two"})
	if err := os.WriteFile(filepath.Join(dir, "last_conversations.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if got := AgyConversationID("/work/one"); got != "session-one" {
		t.Fatal(got)
	}
	if got := AgyConversationID("/work/missing"); got != "" {
		t.Fatalf("resumed unrelated conversation %s", got)
	}
	args := Agy.Coordinator(Options{Continue: true, ResumeID: AgyConversationID("/work/one")})
	if !strings.Contains(strings.Join(args, "\n"), "--conversation\nsession-one") {
		t.Fatal(args)
	}
}

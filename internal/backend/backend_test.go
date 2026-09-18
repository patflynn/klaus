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

func TestCodexPromptControlCharacters(t *testing.T) {
	prompt := "bell\a vertical\v nul\x00 delete\x7f tab\t line\n unicode café"
	argv := Codex.Coordinator(Options{SystemPrompt: prompt})
	for _, arg := range argv {
		if encoded, ok := strings.CutPrefix(arg, "developer_instructions="); ok {
			if strings.Contains(encoded, "\x7f") || strings.Contains(encoded, `\x`) || strings.Contains(encoded, `\a`) || strings.Contains(encoded, `\v`) {
				t.Fatalf("Go-only TOML escapes: %s", encoded)
			}
			var decoded string
			if err := json.Unmarshal([]byte(encoded), &decoded); err != nil || decoded != prompt {
				t.Fatalf("prompt changed: %q, %v", decoded, err)
			}
			return
		}
	}
	t.Fatal("instructions missing")
}

func TestAgyResumeDoesNotRepeatInstructions(t *testing.T) {
	argv := Agy.Coordinator(Options{SystemPrompt: "long original instructions", Continue: true, ResumeID: "conversation"})
	joined := strings.Join(argv, "\n")
	if strings.Contains(joined, "long original instructions") || !strings.Contains(joined, "session resumed") || !strings.Contains(joined, "--conversation\nconversation") {
		t.Fatal(argv)
	}
}

func TestOneShot(t *testing.T) {
	for _, kind := range []Kind{Claude, Codex, Agy} {
		for _, mode := range []string{"one-shot", "new-thread", "resume"} {
			t.Run(string(kind)+"/"+mode, func(t *testing.T) {
				opts := OneShotOptions{AgyAgent: "read-only-agent", Prompt: "question", SystemPrompt: "system", Model: "model", Effort: "high", Threaded: mode != "one-shot"}
				if mode == "new-thread" {
					opts.SessionID = "new-id"
				}
				if mode == "resume" {
					opts.ResumeID = "prior-id"
				}
				argv := OneShot(kind, opts)
				joined := strings.Join(argv, "\n")
				require := func(parts ...string) {
					t.Helper()
					for _, p := range parts {
						if !strings.Contains(joined, p) {
							t.Fatalf("missing %q in %q", p, argv)
						}
					}
				}
				if strings.Contains(joined, "dangerously") || strings.Contains(joined, "stream-json") {
					t.Fatalf("unsafe argv: %q", argv)
				}
				require("--model\nmodel")
				switch kind {
				case Claude:
					require("-p", "--safe-mode", "--output-format\ntext", "--tools\nRead,Grep,Glob", "--permission-mode\ndontAsk", "--strict-mcp-config", "--system-prompt\nsystem", "--effort\nhigh")
					if mode == "one-shot" {
						require("--no-session-persistence")
					} else if strings.Contains(joined, "--no-session-persistence") {
						t.Fatal("thread persistence disabled")
					}
					if mode == "new-thread" {
						require("--session-id\nnew-id")
					}
					if mode == "resume" {
						require("--resume\nprior-id")
					}
				case Codex:
					require("mcp_servers={}", "features.apps=false", "features.plugins=false", "features.hooks=false", "features.multi_agent=false", "exec\n--sandbox\nread-only", `approval_policy="never"`, `developer_instructions="system"`, `model_reasoning_effort="high"`)
					if mode == "one-shot" {
						require("--ephemeral")
					} else if strings.Contains(joined, "--ephemeral") {
						t.Fatal("thread persistence disabled")
					}
					if mode == "resume" {
						require("resume\nprior-id")
					}
					if argv[len(argv)-1] != "-" {
						t.Fatal("prompt must come from stdin")
					}
				case Agy:
					require("--agent\nread-only-agent", "--mode\nplan", "--sandbox", "--output-format\ntext", "--print\nsystem\n\nquestion", "--effort\nhigh")
					if mode == "resume" {
						require("--conversation\nprior-id")
					}
				}
			})
		}
	}
}

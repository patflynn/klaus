package backend

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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

// Every backend takes the prompt on stdin so a brief of any length stays out
// of the tmux command. agy has no system prompt flag, so it reads that there too.
func TestWorkerPromptTransport(t *testing.T) {
	const prompt, system, sysFile = "the whole brief", "system text", "/prompts/run.system.md"
	for _, k := range []Kind{Claude, Codex, Agy} {
		for _, resume := range []string{"", "prior-session"} {
			t.Run(string(k)+"/resume="+resume, func(t *testing.T) {
				args := k.Worker(Options{Prompt: prompt, SystemPrompt: system, SystemPromptFile: sysFile, ResumeID: resume, RunID: "run", Budget: "5"})
				joined := strings.Join(args, "\n")
				if strings.Contains(joined, prompt) {
					t.Fatalf("prompt in argv: %q", args)
				}
				stdin, want := k.Stdin(system, prompt), prompt
				if k == Agy {
					want = system + "\n\n" + prompt
				}
				if stdin != want {
					t.Fatalf("stdin = %q, want %q", stdin, want)
				}
				switch k {
				case Claude:
					if !strings.Contains(joined, "--append-system-prompt-file\n"+sysFile) || strings.Contains(joined, system) {
						t.Fatalf("system prompt not read from its file: %q", args)
					}
				case Codex:
					if args[len(args)-1] != "-" || !strings.Contains(joined, "developer_instructions=") {
						t.Fatalf("codex must read stdin and keep inline instructions: %q", args)
					}
				case Agy:
					// --print would take the next argument as the prompt.
					if slices.Contains(args, "--print") || strings.Contains(joined, system) {
						t.Fatalf("agy must read both prompts on stdin: %q", args)
					}
				}
			})
		}
	}
	args := Claude.Worker(Options{SystemPrompt: system})
	if !strings.Contains(strings.Join(args, "\n"), "--append-system-prompt\n"+system) {
		t.Fatalf("inline system prompt dropped without a file: %q", args)
	}
}

// Claude sessions that can commit or open PRs must run with attribution off,
// whether --settings loads the inline settings or the coordinator's status
// line file: Claude takes one --settings, so the file must carry attribution
// too. A worker always gets the inline settings. The setting is Claude-only,
// so other backends' argv must not carry it.
func TestClaudeAttributionDisabled(t *testing.T) {
	for _, s := range []string{ClaudeSettings, ClaudeSettingsWithStatusLine("widget")} {
		var settings struct {
			Attribution struct {
				Commit, PR *string
				SessionURL *bool `json:"sessionUrl"`
			}
		}
		if err := json.Unmarshal([]byte(s), &settings); err != nil {
			t.Fatal(err)
		}
		if a := settings.Attribution; a.Commit == nil || *a.Commit != "" || a.PR == nil || *a.PR != "" || a.SessionURL == nil || *a.SessionURL {
			t.Fatalf("attribution not disabled: %s", s)
		}
	}
	for _, k := range []Kind{Claude, Codex, Agy} {
		for _, file := range []string{"", "/state/run.settings.json"} {
			for _, resume := range []string{"", "prior-session"} {
				o := Options{SystemPrompt: "system", Prompt: "prompt", RunID: "run", Budget: "5", ResumeID: resume, Continue: resume != "", SettingsFile: file}
				coordinator := ClaudeSettings
				if file != "" {
					coordinator = file
				}
				worker := k.Worker(o)
				for name, argv := range map[string][]string{"worker": worker, "coordinator": k.Coordinator(o)} {
					want := map[string]string{"worker": ClaudeSettings, "coordinator": coordinator}[name]
					joined := strings.Join(argv, "\n")
					has := strings.Contains(joined, "--settings\n"+want+"\n")
					if has != (k == Claude) || strings.Count(joined, "--settings") != map[bool]int{true: 1}[k == Claude] {
						t.Errorf("%s %s file=%q resume=%q: --settings %s present = %v: %q", k, name, file, resume, want, has, argv)
					}
				}
			}
		}
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
				opts := OneShotOptions{AgyAgent: "read-only-agent", SystemPrompt: "system", Model: "model", Effort: "high", Threaded: mode != "one-shot"}
				if mode == "new-thread" {
					opts.SessionID = "new-id"
				}
				if mode == "resume" {
					opts.ResumeID = "prior-id"
				}
				argv, err := OneShot(kind, opts)
				if err != nil {
					t.Fatal(err)
				}
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
					if strings.Contains(joined, "mcp_servers={}") {
						t.Fatalf("ineffective MCP override: %q", argv)
					}
					require("--ignore-user-config", "features.apps=false", "features.plugins=false", "features.hooks=false", "features.multi_agent=false", "--sandbox\nread-only", `approval_policy="never"`, `developer_instructions="system"`, `model_reasoning_effort="high"`)
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
					require("--agent\nread-only-agent", "--mode\nplan", "--sandbox", "--output-format\ntext", "--effort\nhigh")
					if slices.Contains(argv, "--print") || strings.Contains(joined, "system") {
						t.Fatalf("agy must read both prompts on stdin: %q", argv)
					}
					if mode == "resume" {
						require("--conversation\nprior-id")
					}
				}
			})
		}
	}
}

func TestOneShotMisconfiguration(t *testing.T) {
	for _, kind := range []Kind{Agy, Kind("unknown")} {
		if argv, err := OneShot(kind, OneShotOptions{}); err == nil || argv != nil {
			t.Fatalf("%s: argv=%v, err=%v", kind, argv, err)
		}
	}
}

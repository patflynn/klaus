// Package backend describes the CLI contracts used by Klaus coordinators and workers.
package backend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Kind string

const (
	Claude Kind = "claude"
	Codex  Kind = "codex"
	Agy    Kind = "agy"
)

func Parse(s string) (Kind, error) {
	if s == "" {
		return Claude, nil
	}
	switch Kind(s) {
	case Claude, Codex, Agy:
		return Kind(s), nil
	}
	return "", fmt.Errorf("unknown backend %q: choose claude, codex, or agy", s)
}

func (k Kind) ValidateEffort(s string) error {
	if s == "" {
		return nil
	}
	allowed := "low medium high"
	if k == Claude {
		allowed += " xhigh max"
	}
	if k == Codex {
		allowed += " minimal xhigh"
	}
	for _, v := range strings.Fields(allowed) {
		if s == v {
			return nil
		}
	}
	return fmt.Errorf("invalid effort %q for %s: choose %s", s, k, allowed)
}

// ClaudeSettings turns off Claude Code's commit trailer, PR-body line and
// session link. --settings layers this over the user's settings files rather
// than replacing them. Klaus work must not carry AI attribution; the
// commit-msg hook and the agent brief enforce the same rule.
const ClaudeSettings = `{"attribution":{"commit":"","pr":"","sessionUrl":false}}`

// ClaudeSettingsWithStatusLine is ClaudeSettings plus a status line showing
// repoName and the branch checked out where the coordinator runs. klaus
// writes it to a file in the session's state that Options.SettingsFile
// names, never into the worktree, where git would see it. A repository's own
// .claude/settings.json still loads beneath it, untouched. Workers get no
// status line: claude -p, piped into _format-stream, never renders one.
func ClaudeSettingsWithStatusLine(repoName string) string {
	var settings map[string]any
	if err := json.Unmarshal([]byte(ClaudeSettings), &settings); err != nil {
		panic(err)
	}
	settings["statusLine"] = map[string]any{
		"type": "command",
		"command": fmt.Sprintf(
			`input=$(cat); cwd=$(echo "$input" | jq -r '.workspace.current_dir'); branch=$(git -C "$cwd" --no-optional-locks branch --show-current 2>/dev/null); if [ -n "$branch" ]; then echo "%s ($branch)"; else echo "%s"; fi`,
			repoName, repoName,
		),
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(out) + "\n"
}

type Options struct {
	SystemPrompt, Prompt, RunID, ResumeID, Model, Effort, Budget string
	// SystemPromptFile replaces SystemPrompt for Claude workers; other backends have no file flag.
	SystemPromptFile string
	// SettingsFile replaces the inline ClaudeSettings for a Claude coordinator
	// with a file holding ClaudeSettingsWithStatusLine. Claude takes one
	// --settings, so the file carries ClaudeSettings too. Workers always get
	// the inline settings; other backends have no settings flag.
	SettingsFile string
	Continue     bool
}

// Worker returns argv, without shell interpolation. The prompt is never in
// argv: the caller feeds the worker k.Stdin(o.SystemPrompt, o.Prompt) on
// stdin, which keeps long briefs out of the tmux command. Dollar budgets are
// a Claude capability.
func (k Kind) Worker(o Options) []string {
	var a []string
	switch k {
	case Claude:
		a = []string{"claude", "-p", "-n", o.RunID}
		if o.ResumeID != "" {
			a = append(a, "--resume", o.ResumeID, "--fork-session")
		}
		a = append(a, "--dangerously-skip-permissions", "--verbose", "--output-format", "stream-json", "--max-budget-usd", o.Budget, "--settings", ClaudeSettings)
		if o.SystemPromptFile != "" {
			a = append(a, "--append-system-prompt-file", o.SystemPromptFile)
		} else {
			a = append(a, "--append-system-prompt", o.SystemPrompt)
		}
	case Codex:
		a = []string{"codex", "exec"}
		if o.ResumeID != "" {
			a = append(a, "fork", o.ResumeID)
		}
		a = append(a, "--json", "--dangerously-bypass-approvals-and-sandbox", "-c", "developer_instructions="+tomlString(o.SystemPrompt))
	case Agy:
		a = []string{"agy", "--add-dir", ".", "--output-format", "stream-json", "--dangerously-skip-permissions", "--print-timeout", "24h"}
	}
	a = k.modelArgs(a, o)
	if k == Codex {
		// "-" reads stdin; exec fork needs it spelled out.
		return append(a, "-")
	}
	// claude -p reads stdin when no prompt argument is given. agy, given no
	// --print, runs print mode on the prompt it reads from a stdin that isn't
	// a terminal; --print "-" would be taken as the literal prompt.
	return a
}

// need is a command a backend's worker runs by name, and why.
type need struct{ command, why string }

// needs are the commands each backend's worker runs by name, so they must
// resolve on the PATH it gets: its own CLI, and for agy, bash, which runs
// every command agy's run_command tool runs. An agy worker without bash
// can't run any command, only edit files.
func (k Kind) needs() []need {
	cli := need{string(k), "the worker's own CLI"}
	if k == Agy {
		return []need{cli, {"bash", "agy runs every command through bash"}}
	}
	return []need{cli}
}

// CheckPath returns an error naming the first command k's worker needs that
// doesn't resolve on path, a PATH value, for a worker that runs in dir, and
// the PATH it searched.
func (k Kind) CheckPath(path, dir string) error {
	for _, n := range k.needs() {
		if !onPath(n.command, path, dir) {
			return fmt.Errorf("the %s worker can't run: %s isn't on the PATH it gets (%s); searched PATH=%q", k, n.command, n.why, path)
		}
	}
	return nil
}

// onPath reports whether name resolves to an executable file in a directory
// of path, as a shell's PATH lookup finds it in cwd: an empty or relative
// entry is taken from cwd.
func onPath(name, path, cwd string) bool {
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return true
		}
	}
	return false
}

// Stdin is what a worker or a one-shot call reads on stdin: the prompt, with
// the system prompt ahead of it for agy, which has no flag for one.
func (k Kind) Stdin(systemPrompt, prompt string) string {
	if k == Agy {
		return systemPrompt + "\n\n" + prompt
	}
	return prompt
}

func (k Kind) Coordinator(o Options) []string {
	var a []string
	switch k {
	case Claude:
		settings := ClaudeSettings
		if o.SettingsFile != "" {
			settings = o.SettingsFile
		}
		a = []string{"claude", "--dangerously-skip-permissions", "-n", o.RunID, "--settings", settings, "--append-system-prompt", o.SystemPrompt}
		if o.Continue {
			if o.ResumeID != "" {
				a = append(a, "--resume", o.ResumeID)
			} else {
				a = append(a, "--continue")
			}
		} else if o.ResumeID != "" {
			a = append(a, "--session-id", o.ResumeID)
		}
	case Codex:
		a = []string{"codex"}
		if o.Continue {
			// Klaus allocates a distinct workspace per coordinator; do not add --all.
			a = append(a, "resume", "--last")
		}
		a = append(a, "--dangerously-bypass-approvals-and-sandbox", "-c", "developer_instructions="+tomlString(o.SystemPrompt))
	case Agy:
		prompt := o.SystemPrompt
		if o.Continue && o.ResumeID != "" {
			prompt = "Klaus coordinator session resumed. Continue using the existing coordinator instructions; use klaus status to refresh worker state. Your background klaus watch ended with the last session: start the command on the latest next: line it printed again as a background task, or, if no watch printed one, the first watch command your instructions give."
		}
		a = []string{"agy", "--add-dir", ".", "--dangerously-skip-permissions", "--prompt-interactive", prompt}
		if o.Continue && o.ResumeID != "" {
			a = append(a, "--conversation", o.ResumeID)
		}
	}
	return k.modelArgs(a, o)
}

func (k Kind) modelArgs(a []string, o Options) []string {
	if o.Model != "" {
		a = append(a, "--model", o.Model)
	}
	if o.Effort != "" {
		if k == Codex {
			a = append(a, "-c", "model_reasoning_effort="+tomlString(o.Effort))
		} else {
			a = append(a, "--effort", o.Effort)
		}
	}
	return a
}

// ShellCommand quotes every argument, including multiline prompts and model names.
func ShellCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
	}
	return strings.Join(quoted, " ")
}

// JSON string quoting uses TOML-compatible Unicode escapes for control characters.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return strings.ReplaceAll(string(b), "\x7f", `\u007f`)
}

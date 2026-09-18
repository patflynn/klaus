// Package backend describes the CLI contracts used by Klaus coordinators and workers.
package backend

import (
	"encoding/json"
	"fmt"
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

type Options struct {
	SystemPrompt, Prompt, RunID, ResumeID, Model, Effort, Budget string
	Continue                                                     bool
}

// Worker returns argv, without shell interpolation. Dollar budgets are a Claude capability.
func (k Kind) Worker(o Options) []string {
	var a []string
	switch k {
	case Claude:
		a = []string{"claude", "-p", "-n", o.RunID}
		if o.ResumeID != "" {
			a = append(a, "--resume", o.ResumeID, "--fork-session")
		}
		a = append(a, "--dangerously-skip-permissions", "--verbose", "--output-format", "stream-json", "--max-budget-usd", o.Budget, "--append-system-prompt", o.SystemPrompt)
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
	if k == Agy {
		return append(a, "--print", o.SystemPrompt+"\n\n"+o.Prompt)
	}
	return append(a, o.Prompt)
}

func (k Kind) Coordinator(o Options) []string {
	var a []string
	switch k {
	case Claude:
		a = []string{"claude", "--dangerously-skip-permissions", "-n", o.RunID, "--append-system-prompt", o.SystemPrompt}
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
			prompt = "Klaus coordinator session resumed. Continue using the existing coordinator instructions; use klaus status to refresh worker state."
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

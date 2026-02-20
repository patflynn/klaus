package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

// Config holds the klaus configuration.
type Config struct {
	WorktreeBase  string `json:"worktree_base"`
	DefaultBudget string `json:"default_budget"`
	DataRef       string `json:"data_ref"`
	DefaultBranch string `json:"default_branch"`
}

// Defaults returns a Config with default values.
func Defaults() Config {
	return Config{
		WorktreeBase:  filepath.Join(os.TempDir(), "klaus-sessions"),
		DefaultBudget: "5.00",
		DataRef:       "refs/klaus/data",
		DefaultBranch: "main",
	}
}

// Load reads config from .klaus/config.json in the given repo root.
// If the file doesn't exist, returns defaults.
func Load(repoRoot string) (Config, error) {
	cfg := Defaults()
	path := filepath.Join(repoRoot, ".klaus", "config.json")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading config: %w", err)
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config: %w", err)
	}
	return cfg, nil
}

// PromptVars are the template variables available in the system prompt.
type PromptVars struct {
	RunID    string
	Issue    string
	Branch   string
	RepoName string
}

// RenderPrompt renders the system prompt template from .klaus/prompt.md.
// If the file doesn't exist, returns a default prompt.
func RenderPrompt(repoRoot string, vars PromptVars) (string, error) {
	path := filepath.Join(repoRoot, ".klaus", "prompt.md")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return renderDefaultPrompt(vars)
		}
		return "", fmt.Errorf("reading prompt template: %w", err)
	}

	return renderTemplate(string(data), vars)
}

// Init scaffolds the .klaus/ directory with default config and prompt template.
func Init(repoRoot string) error {
	dir := filepath.Join(repoRoot, ".klaus")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating .klaus dir: %w", err)
	}

	// Write default config
	cfg := Defaults()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	// Write default prompt template
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte(defaultPromptTemplate), 0o644); err != nil {
		return fmt.Errorf("writing prompt template: %w", err)
	}

	return nil
}

const defaultPromptTemplate = `You are an autonomous agent working on this repository.

## Workflow
1. Make your changes in this worktree.
2. Run ` + "`git add`" + ` on any new files.
3. Test your changes.
4. Create a focused git commit.
5. Push your branch: ` + "`git push -u origin HEAD`" + `
6. Create a PR. Include the following footer at the bottom of the PR body:
   Run: {{.RunID}}{{if .Issue}}
   Fixes #{{.Issue}}{{end}}

## Conventions
- Never commit directly to the default branch — always use a PR branch.
`

const defaultSessionPromptTemplate = `You are a coordinator running inside a klaus session (session ID: {{.RunID}}).

Your working directory is an isolated git worktree on branch {{.Branch}} for repo {{.RepoName}}.

## Delegating work

You should delegate implementation work to autonomous agents rather than doing it directly.
Each launched agent gets its own isolated worktree and branch, and will create a PR when done.

To delegate implementation tasks to autonomous agents:
` + "```" + `
klaus launch "<prompt>"
` + "```" + `

To delegate tasks referencing a GitHub issue:
` + "```" + `
klaus launch --issue <number> "<prompt>"
` + "```" + `

## Managing agents

- Check on running agents: ` + "`klaus status`" + `
- View agent output: ` + "`klaus logs <run-id>`" + `
- Clean up finished runs: ` + "`klaus cleanup <run-id>`" + `
`

// RenderSessionPrompt renders the session coordinator system prompt.
// It reads from .klaus/session-prompt.md, falling back to the built-in default.
func RenderSessionPrompt(repoRoot string, vars PromptVars) (string, error) {
	path := filepath.Join(repoRoot, ".klaus", "session-prompt.md")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return renderTemplate(defaultSessionPromptTemplate, vars)
		}
		return "", fmt.Errorf("reading session prompt template: %w", err)
	}

	return renderTemplate(string(data), vars)
}

// WriteClaudeSettings writes a .claude/settings.json into the given worktree
// directory with a statusLine that displays the repo name and current branch.
// If the file already exists (e.g. from the checked-out branch), it merges the
// statusLine key into the existing settings.
func WriteClaudeSettings(worktreeDir, repoName string) error {
	claudeDir := filepath.Join(worktreeDir, ".claude")
	settingsPath := filepath.Join(claudeDir, "settings.json")

	statusLine := fmt.Sprintf(
		`input=$(cat); cwd=$(echo "$input" | jq -r '.workspace.current_dir'); branch=$(git -C "$cwd" --no-optional-locks branch --show-current 2>/dev/null); if [ -n "$branch" ]; then echo "%s ($branch)"; else echo "%s"; fi`,
		repoName, repoName,
	)

	var settings map[string]any

	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			settings = make(map[string]any)
		}
	} else {
		settings = make(map[string]any)
	}

	settings["statusLine"] = statusLine

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}

	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("creating .claude dir: %w", err)
	}

	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing settings: %w", err)
	}

	return nil
}

func renderDefaultPrompt(vars PromptVars) (string, error) {
	return renderTemplate(defaultPromptTemplate, vars)
}

func renderTemplate(tmplStr string, vars PromptVars) (string, error) {
	tmpl, err := template.New("prompt").Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return buf.String(), nil
}

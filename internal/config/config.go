package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// Config holds the klaus configuration.
type Config struct {
	WorktreeBase     string   `json:"worktree_base"`
	DefaultBudget    string   `json:"default_budget"`
	DataRef          string   `json:"data_ref"`
	DefaultBranch    string   `json:"default_branch"`
	TrustedReviewers []string `json:"trusted_reviewers"`
}

// Defaults returns a Config with default values.
func Defaults() Config {
	return Config{
		WorktreeBase:     filepath.Join(os.TempDir(), "klaus-sessions"),
		DefaultBudget:    "5.00",
		DataRef:          "refs/klaus/data",
		DefaultBranch:    "main",
		TrustedReviewers: []string{"gemini-code-assist[bot]"},
	}
}

// Load reads config by layering: defaults → ~/.klaus/config.json → .klaus/config.json.
// Repo-local config overrides global config.
func Load(repoRoot string) (Config, error) {
	cfg := Defaults()

	// Layer 1: global config from ~/.klaus/config.json
	if home, err := os.UserHomeDir(); err == nil {
		globalPath := filepath.Join(home, ".klaus", "config.json")
		if data, err := os.ReadFile(globalPath); err == nil {
			if err := json.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parsing global config: %w", err)
			}
		}
	}

	// Layer 2: repo-local config from .klaus/config.json (overrides global)
	if repoRoot != "" {
		localPath := filepath.Join(repoRoot, ".klaus", "config.json")
		data, err := os.ReadFile(localPath)
		if err != nil {
			if os.IsNotExist(err) {
				return cfg, nil
			}
			return cfg, fmt.Errorf("reading config: %w", err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parsing config: %w", err)
		}
	}

	return cfg, nil
}

// PromptVars are the template variables available in the system prompt.
type PromptVars struct {
	RunID    string
	Issue    string
	Branch   string
	RepoName string
	PR       string
	Projects string // Formatted list of registered projects for session prompt
}

// RenderPrompt renders the system prompt template from .klaus/prompt.md.
// If the file doesn't exist, returns a default prompt.
func RenderPrompt(repoRoot string, vars PromptVars) (string, error) {
	return renderPromptFromFile(repoRoot, "prompt.md", defaultPromptTemplate, vars)
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

// InitGlobal scaffolds ~/.klaus/config.json with default configuration.
// Used when running outside a git repository.
func InitGlobal() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home dir: %w", err)
	}
	dir := filepath.Join(home, ".klaus")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating ~/.klaus dir: %w", err)
	}

	cfg := Defaults()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing config: %w", err)
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

## Testing
- Prefer integration and e2e tests that exercise real behavior over unit tests with mocked internals.
- Only unit test genuinely tricky logic. Don't write tests that just mirror the implementation.
- A few tests that run the real binary or real commands are worth more than many tests with injected fakes.
- Run ` + "`go build ./...`" + ` before committing to verify compilation.
- Run the project's test suite before creating a PR. Fix failures before pushing.

## Conventions
- Never commit directly to the default branch — always use a PR branch.

## Documentation
- If you add or change a CLI command or flag, update the help text in the cobra command definition.
- If you add or change user-facing behavior, update the README if one exists.
- Keep code comments accurate — update or remove comments that no longer match the code.
`

const defaultSessionPromptTemplate = `You are a coordinator running inside a klaus session (session ID: {{.RunID}}).
{{if .RepoName}}
Your working directory is an isolated git worktree on branch {{.Branch}} for repo {{.RepoName}}.
{{else}}
Your working directory is a scratch workspace (no git repo). Use --repo owner/repo with
klaus launch to target specific repositories.
{{end}}
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
{{if not .RepoName}}
When not in a git repo, you must specify a target repository. You can either
set a session-level default or specify it per launch:
` + "```" + `
klaus target owner/repo              # set default for this session
klaus launch "<prompt>"              # uses the target
klaus launch --repo owner/repo "<prompt>"  # override per launch
` + "```" + `
{{end}}
## Managing agents

- Check on running agents: ` + "`klaus status`" + `
- View agent output: ` + "`klaus logs <run-id>`" + `
- Clean up finished runs: ` + "`klaus cleanup <run-id>`" + `
- Set default target repo: ` + "`klaus target owner/repo`" + ` or ` + "`klaus target <project-name>`" + `
- Show current target: ` + "`klaus target`" + `
{{if .Projects}}
## Registered projects

These projects are available by name with ` + "`klaus launch --repo <name>`" + ` or ` + "`klaus target <name>`" + `:
{{.Projects}}
{{end}}## Testing
- Ensure launched agents prefer integration and e2e tests over mocked unit tests.
- Tests should exercise real behavior — a few tests that run the real binary are worth more than many with injected fakes.
- Only unit test genuinely tricky logic.
- Run the project's test suite before creating a PR.
- If tests fail, fix them before proceeding.
`

// RenderSessionPrompt renders the session coordinator system prompt.
// It reads from .klaus/session-prompt.md, falling back to the built-in default.
func RenderSessionPrompt(repoRoot string, vars PromptVars) (string, error) {
	return renderPromptFromFile(repoRoot, "session-prompt.md", defaultSessionPromptTemplate, vars)
}

// FormatProjectList formats a map of project name → local path as a bulleted list
// for use in the session prompt. Returns empty string if no projects are registered.
func FormatProjectList(projects map[string]string) string {
	if len(projects) == 0 {
		return ""
	}
	var lines []string
	for name, localPath := range projects {
		lines = append(lines, fmt.Sprintf("- %s (%s)", name, contractHomeForDisplay(localPath)))
	}
	// Sort for deterministic output
	sortStrings(lines)
	return strings.Join(lines, "\n")
}

// contractHomeForDisplay replaces the home directory prefix with ~ for display.
func contractHomeForDisplay(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// sortStrings sorts a slice of strings in place.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

const defaultWatchPromptTemplate = `You are an autonomous CI monitoring agent for PR #{{.PR}} in this repository.

## Your Mission

Monitor CI checks for PR #{{.PR}}, diagnose any failures, fix them, and push
fixes until all checks pass. Also handle merge conflicts and review comments.

## Workflow

### 1. Check CI status

Run this command to see the current state of all checks:
` + "```" + `
gh pr checks {{.PR}}
` + "```" + `

If checks are still running, wait 30 seconds and check again.

### 2. Check for merge conflicts

After checking CI, check for merge conflicts:
` + "```" + `
gh pr view {{.PR}} --json mergeable -q .mergeable
` + "```" + `

If the result is "CONFLICTING", rebase onto the base branch:
` + "```" + `
git fetch origin main
git rebase origin/main
` + "```" + `

If the rebase succeeds, run the build/tests to verify, then force push:
` + "```" + `
git push --force-with-lease
` + "```" + `

If the rebase fails, run ` + "`git rebase --abort`" + `, log a warning, and continue
monitoring other issues.

### 3. Check review comments

Fetch PR review comments to understand requested changes:
` + "```" + `
gh api repos/{owner}/{repo}/pulls/{{.PR}}/comments
` + "```" + `

Review comments may contain actionable feedback. Address them alongside any
CI failures.

### 4. When a check fails

Identify the failed workflow run. Get the run ID from the checks output, then
read the failure logs:
` + "```" + `
gh run view <run-id> --log-failed
` + "```" + `

Analyze the logs to understand the root cause.

### 5. Fix the issue

- Read the relevant source and test files to understand the failure.
- Also address any actionable PR review comments.
- Make the minimal code change needed to fix the CI failure.
- Run any available local test commands to verify your fix before pushing.
- Stage your changes with ` + "`git add`" + `.
- Create a focused commit describing the fix.
- Push: ` + "`git push`" + `

### 6. Reply to addressed review comments

After pushing a fix, reply to each review comment you addressed:
` + "```" + `
gh api repos/{owner}/{repo}/pulls/{{.PR}}/comments/{comment-id}/replies -f body="Addressed in <commit-sha>"
` + "```" + `

Replace ` + "`<commit-sha>`" + ` with the actual commit hash from your push.

### 7. Repeat

After pushing, wait for CI to restart (check with ` + "`gh pr checks {{.PR}}`" + `),
then monitor again. Continue until all checks pass.

## Testing
- Prefer integration and e2e tests that exercise real behavior over unit tests with mocked internals.
- Only unit test genuinely tricky logic.
- Run the project's test suite before creating a PR.
- If tests fail, fix them before proceeding.

## Guidelines

- Only fix CI failures and address review comments — do not make unrelated changes.
- If a fix attempt does not resolve the issue after 3 tries, report what you
  have found and stop.
- Read test files and source code as needed to understand failures.
- Keep commits small and focused on the specific failure.

## Identity

Run: {{.RunID}}
Branch: {{.Branch}}
`

// RenderWatchPrompt renders the watch agent system prompt.
// It reads from .klaus/watch-prompt.md, falling back to the built-in default.
func RenderWatchPrompt(repoRoot string, vars PromptVars) (string, error) {
	return renderPromptFromFile(repoRoot, "watch-prompt.md", defaultWatchPromptTemplate, vars)
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

	settings["statusLine"] = map[string]any{
		"type":    "command",
		"command": statusLine,
	}

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

// PreTrustWorktree creates a Claude Code project entry for the given directory
// so that the workspace trust dialog is not shown when launching Claude
// interactively. Claude Code stores per-project data in
// ~/.claude/projects/<encoded-path>/ and shows a trust dialog for directories
// without an existing project entry.
func PreTrustWorktree(worktreeDir string) error {
	absPath, err := filepath.Abs(worktreeDir)
	if err != nil {
		return fmt.Errorf("resolving worktree path: %w", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("finding home dir: %w", err)
	}

	// Claude Code encodes paths by replacing path separators with hyphens.
	encoded := strings.ReplaceAll(absPath, string(filepath.Separator), "-")
	projectDir := filepath.Join(homeDir, ".claude", "projects", encoded)

	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return fmt.Errorf("creating project dir: %w", err)
	}

	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if _, err := os.Stat(indexPath); err == nil {
		return nil // already exists
	}

	index := map[string]any{
		"version":      1,
		"entries":      []any{},
		"originalPath": absPath,
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling sessions index: %w", err)
	}
	if err := os.WriteFile(indexPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing sessions index: %w", err)
	}
	return nil
}

// DefaultPrinciples is used when .klaus/principles.md doesn't exist.
const DefaultPrinciples = `# Project Principles

## Tech Defaults
- Go for CLI tools and backend services
- TypeScript for web applications
- Tailwind CSS for styling in web applications
- These are defaults — override based on project needs

## Dependencies
- Minimal dependencies. Prefer stdlib and zero-dep solutions.
- Every dependency is a maintenance and supply chain liability.
- Vanilla over frameworks unless complexity genuinely warrants one.

## Environment
- Use nix flakes for environment management (flake.nix with dev shell)
- Include klaus in the dev shell: use 'github:patflynn/klaus' as a flake input
- No global tool assumptions — everything through nix

## Testing
- Automated e2e testing from day one
- Tests should exercise the real thing, not just unit tests
- Playwright for web projects, go test for Go projects
- CI must run tests on every PR

## Security & Supply Chain
- SLSA-style CI/CD pipelines in GitHub Actions
- Use zizmor to verify GitHub Actions are configured safely
- No secrets in code, privacy-conscious data handling
- Pin dependencies to exact versions

## CI/CD
- GitHub Actions for CI
- GitHub Pages for static web apps (PWA pattern, no server infrastructure unless needed)
- CI checks: build, test, lint where appropriate

## Code Quality
- Tight, well-factored modules
- Low overall system complexity
- Don't implement features if ongoing maintenance cost is too high
- Three lines of duplicated code beats a premature abstraction

## Project Setup
- CLAUDE.md with project conventions and rules
- .klaus/config.json for klaus integration
- README.md with setup instructions
- .gitignore appropriate to the language
`

// LoadPrinciples reads .klaus/principles.md from the given directory.
// If the file doesn't exist, returns DefaultPrinciples.
func LoadPrinciples(dir string) (string, error) {
	path := filepath.Join(dir, ".klaus", "principles.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultPrinciples, nil
		}
		return "", fmt.Errorf("reading principles: %w", err)
	}
	return string(data), nil
}

func renderPromptFromFile(repoRoot, filename, defaultTemplate string, vars PromptVars) (string, error) {
	if repoRoot == "" {
		return renderTemplate(defaultTemplate, vars)
	}

	path := filepath.Join(repoRoot, ".klaus", filename)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return renderTemplate(defaultTemplate, vars)
		}
		return "", fmt.Errorf("reading %s: %w", filename, err)
	}

	return renderTemplate(string(data), vars)
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

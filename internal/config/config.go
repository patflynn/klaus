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
	WorktreeBase        string           `json:"worktree_base"`
	DefaultBudget       string           `json:"default_budget"`
	DataRef             string           `json:"data_ref"`
	DefaultBranch       string           `json:"default_branch"`
	TrustedReviewers    []string         `json:"trusted_reviewers"`
	RequireApproval     *bool            `json:"require_approval,omitempty"`
	AutoMergeOnApproval *bool            `json:"auto_merge_on_approval,omitempty"`
	PreReview           *PreReviewConfig `json:"pre_review,omitempty"`
	SandboxHost         string           `json:"sandbox_host,omitempty"`
}

// PreReviewConfig configures the pre-PR review checks.
type PreReviewConfig struct {
	Enabled      *bool    `json:"enabled,omitempty"`         // default: true
	Linters      []string `json:"linters,omitempty"`         // default: auto-detect from project
	ReviewModel  string   `json:"review_model,omitempty"`    // default: "haiku"
	MaxFixRounds int      `json:"max_fix_rounds,omitempty"`  // default: 2
	BlockOn      string   `json:"block_on,omitempty"`        // default: "high" (block PR on high+ findings)
}

// RequiresApproval returns true if approval is required before merging.
// Defaults to true if not configured.
func (c *Config) RequiresApproval() bool {
	if c.RequireApproval == nil {
		return true
	}
	return *c.RequireApproval
}

// AutoMergesOnApproval returns true if PRs should be automatically merged
// upon approval. Defaults to false if not configured.
func (c *Config) AutoMergesOnApproval() bool {
	if c.AutoMergeOnApproval == nil {
		return false
	}
	return *c.AutoMergeOnApproval
}

// PreReviewEnabled returns whether pre-review is enabled (default: true).
func (c *Config) PreReviewEnabled() bool {
	if c.PreReview == nil || c.PreReview.Enabled == nil {
		return true
	}
	return *c.PreReview.Enabled
}

// PreReviewLinters returns configured linters, or empty if none configured.
func (c *Config) PreReviewLinters() []string {
	if c.PreReview == nil {
		return nil
	}
	return c.PreReview.Linters
}

// PreReviewModel returns the configured review model, or "haiku" by default.
func (c *Config) PreReviewModel() string {
	if c.PreReview == nil || c.PreReview.ReviewModel == "" {
		return "haiku"
	}
	return c.PreReview.ReviewModel
}

// PreReviewMaxFixRounds returns the max fix rounds, or 2 by default.
func (c *Config) PreReviewMaxFixRounds() int {
	if c.PreReview == nil || c.PreReview.MaxFixRounds == 0 {
		return 2
	}
	return c.PreReview.MaxFixRounds
}

// PreReviewBlockOn returns the minimum severity to block on, or "high" by default.
func (c *Config) PreReviewBlockOn() string {
	if c.PreReview == nil || c.PreReview.BlockOn == "" {
		return "high"
	}
	return c.PreReview.BlockOn
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

Do not include any AI attribution or 'Generated with Claude Code' lines in the PR body.

## Testing
- Prefer integration and e2e tests that exercise real behavior over unit tests with mocked internals.
- Only unit test genuinely tricky logic. Don't write tests that just mirror the implementation.
- A few tests that run the real binary or real commands are worth more than many tests with injected fakes.
- Run ` + "`go build ./...`" + ` before committing to verify compilation.
- Run the project's test suite before creating a PR. Fix failures before pushing.

## Pre-PR Review
- Before creating a PR, run ` + "`klaus _pre-review`" + ` in your worktree.
- If it reports critical or high findings, fix them before proceeding with the PR.

## Conventions
- Never commit directly to the default branch — always use a PR branch.

## Documentation
- If you add or change a CLI command or flag, update the help text in the cobra command definition.
- If you add or change user-facing behavior, update the README if one exists.
- Keep code comments accurate — update or remove comments that no longer match the code.
`

const defaultPRFixPromptTemplate = `You are an autonomous agent pushing fixes to PR #{{.PR}} in this repository.

## Workflow
1. Make your changes in this worktree.
2. Run ` + "`git add`" + ` on any new files.
3. Test your changes.
4. Create a focused git commit.
5. Push your changes: ` + "`git push`" + `

**IMPORTANT: Do NOT create a new PR. You are working on an existing PR branch ({{.Branch}}).
Just commit and push — the PR will update automatically.**
{{if .Issue}}
Reference issue #{{.Issue}} in your commit messages where appropriate.
{{end}}
## Testing
- Prefer integration and e2e tests that exercise real behavior over unit tests with mocked internals.
- Only unit test genuinely tricky logic. Don't write tests that just mirror the implementation.
- A few tests that run the real binary or real commands are worth more than many tests with injected fakes.
- Run ` + "`go build ./...`" + ` before committing to verify compilation.
- Run the project's test suite before pushing. Fix failures before pushing.

## Conventions
- Never commit directly to the default branch — always use a PR branch.

## Documentation
- If you add or change a CLI command or flag, update the help text in the cobra command definition.
- If you add or change user-facing behavior, update the README if one exists.
- Keep code comments accurate — update or remove comments that no longer match the code.

## Identity
Run: {{.RunID}}
Branch: {{.Branch}}
PR: #{{.PR}}
`

// RenderPRFixPrompt renders the system prompt for PR-fix mode.
// It reads from .klaus/pr-fix-prompt.md, falling back to the built-in default.
func RenderPRFixPrompt(repoRoot string, vars PromptVars) (string, error) {
	return renderPromptFromFile(repoRoot, "pr-fix-prompt.md", defaultPRFixPromptTemplate, vars)
}

const defaultSessionPromptTemplate = `You are a **coordinator** running inside a klaus session (session ID: {{.RunID}}).
{{if .RepoName}}
Your working directory is an isolated git worktree on branch {{.Branch}} for repo {{.RepoName}}.
{{else}}
Your working directory is a scratch workspace (no git repo). Use --repo owner/repo with
klaus launch to target specific repositories.
{{end}}
## Your role

Your PRIMARY job is to **delegate implementation work to agents** via ` + "`klaus launch`" + `.
You do NOT implement code changes directly. Each agent gets its own worktree, branch, and
will create a PR when done. You are the planner and researcher; agents are the builders.

### What you SHOULD do directly
- **Research**: read files, grep code, explore the repo, read GitHub issues/PRs
- **Plan**: break complex work into agent-sized tasks
- **Write detailed agent prompts** using context from your research
- **Answer questions** about the codebase
- **Approve and merge** PRs once agents complete their work

### What you should NOT do
- Implement code changes yourself — delegate to agents
- Launch agents with vague one-line prompts — research first, then write rich prompts
- Monitor agent status manually — the dashboard handles visibility
- Manage the PR merge pipeline — the pipeline controller handles CI monitoring, review comment detection, and auto-dispatch of fix agents

## Launching agents

` + "```" + `
klaus launch "<prompt>"                     # launch an agent
klaus launch --issue <number> "<prompt>"    # reference a GitHub issue
klaus launch --pr <number> "<prompt>"       # push fixes to an existing PR
klaus launch --budget <usd> "<prompt>"      # set a spend cap
` + "```" + `

**Always use --issue when working on a GitHub issue.** The agent needs the issue context.
{{if not .RepoName}}
When not in a git repo, you must specify a target repository:
` + "```" + `
klaus target owner/repo              # set default for this session
klaus launch "<prompt>"              # uses the target
klaus launch --repo owner/repo "<prompt>"  # override per launch
` + "```" + `
{{end}}
## Writing good agent prompts

The prompt you pass to ` + "`klaus launch`" + ` is the agent's entire briefing. The agent has access
to the repo but no other context. A vague prompt produces vague work.

**Before launching**, research the problem: read relevant files, grep for symbols, check
the issue thread. Then write a prompt that includes:

1. **What** needs to change and **why** (not just "fix the bug")
2. **Specific file paths and function names** you found during research
3. **Acceptance criteria** — what does "done" look like?
4. **Constraints** — e.g., "don't change the public API", "add integration tests"

Example — **bad prompt**:
` + "```" + `
klaus launch "fix the auth bug"
` + "```" + `

Example — **good prompt**:
` + "```" + `
klaus launch --issue 42 "The JWT validation in internal/auth/verify.go:ValidateToken()
silently accepts expired tokens because the time comparison on line 87 uses Before()
instead of After(). Fix the comparison and add a test in internal/auth/verify_test.go
that confirms expired tokens are rejected. See issue #42 for the user report."
` + "```" + `

## Managing agents

- ` + "`klaus status`" + ` — check on running agents
- ` + "`klaus logs <run-id>`" + ` — view agent output
- ` + "`klaus cleanup <run-id>`" + ` — clean up finished runs
- ` + "`klaus target [owner/repo | project-name]`" + ` — get/set default target repo
- ` + "`klaus approve <pr-number> [...]`" + ` — approve PRs for merging
- ` + "`klaus merge <pr-number> [...]`" + ` — merge PRs sequentially
- ` + "`klaus track <pr-number> [--repo <repo>]`" + ` — add existing PR to dashboard for pipeline monitoring
- ` + "`klaus untrack <pr-number>`" + ` — stop tracking a PR
- ` + "`klaus dashboard`" + ` — open live dashboard

## How the pipeline works

The dashboard runs a pipeline controller that automatically:
- Monitors CI status for all tracked PRs (every 30s)
- Dispatches fix agents when CI fails (via ` + "`klaus launch --pr`" + `)
- Dispatches fix agents when trusted reviewers (e.g. gemini-code-assist) leave comments
- Auto-merges PRs when CI passes + approved + no conflicts (if ` + "`auto_merge_on_approval`" + ` is configured)
- Tracks pipeline stages per PR: ci_pending → ci_failed → ci_passed → review_pending → approved → merging → merged

You do NOT need to manually monitor CI or dispatch fix agents — the pipeline handles routine failures. Focus on:
- Initial research and agent dispatch for new work
- Reviewing agent output when the pipeline stalls
- Approving PRs that are ready (` + "`klaus approve`" + `)

## State and tracking

- Each agent run creates a state file at ` + "`~/.klaus/sessions/{session-id}/runs/{run-id}.json`" + `
- The dashboard watches this directory via fsnotify — new/modified files appear instantly
- To add an existing PR without launching an agent, use ` + "`klaus track <pr-number> --repo <repo>`" + `
- Run states track: ID, prompt, branch, worktree, PR URL, type, target repo, cost, duration, approval/merge status
- State types: ` + "`session`" + ` (coordinator), ` + "`launch`" + ` (agent), ` + "`pr-fix`" + ` (fix agent), ` + "`track`" + ` (monitored PR)

## Approval and merge workflow

- PRs require approval before merging (configurable via ` + "`require_approval`" + ` in .klaus/config.json)
- ` + "`klaus approve <pr-number>`" + ` — mark a PR ready for merge
- ` + "`klaus approve --all`" + ` — approve all merge-ready PRs
- ` + "`klaus merge <pr-number> [...]`" + ` — merge PRs sequentially with auto-rebase
- Merge handles: CI check, conflict detection, automatic rebase if needed, 10-min CI poll timeout
- The pipeline can auto-merge approved PRs if ` + "`auto_merge_on_approval`" + ` is enabled in config

## Agent lifecycle

What happens after ` + "`klaus launch`" + `:
1. Creates an isolated git worktree and tmux pane
2. Agent runs Claude Code in the pane, working on the branch
3. When done, ` + "`_finalize`" + ` extracts cost/duration/PR URL from the log
4. Events are emitted (agent:completed, agent:pr-created)
5. Dashboard detects state change and updates display
6. Pipeline evaluates the PR and manages CI/review/merge cycle

## Gotchas and common issues

- **Worktree conflicts**: When using --pr to push fixes to an existing PR, the PR's branch cannot be checked out in the main repo clone. If you get a worktree error, switch the main repo to main first.
- **Repo resolution**: Outside a git repo, always use --repo or set a target. Priority: explicit --repo flag > session target > current git repo.
- **Agent prompts are the only context**: Agents have access to the repo but NO conversation history. Everything the agent needs must be in the prompt — file paths, function names, acceptance criteria, constraints.
- **State from other sessions**: Run states from previous sessions are NOT visible in the current session's dashboard. To track a PR from a previous session, use ` + "`klaus track`" + `.
{{if .Projects}}
## Registered projects

These projects are available by name with ` + "`klaus launch --repo <name>`" + ` or ` + "`klaus target <name>`" + `:
{{.Projects}}
{{end}}
## Testing guidance for agents
- Ensure launched agents prefer integration and e2e tests over mocked unit tests.
- Tests should exercise real behavior — a few tests that run the real binary are worth more than many with injected fakes.
- Only unit test genuinely tricky logic.
- Agents must run the project's test suite before creating a PR and fix any failures.
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
// interactively. It does two things:
//  1. Creates ~/.claude/projects/<encoded-path>/sessions-index.json
//  2. Sets hasTrustDialogAccepted=true in the Claude Code config file
//     (~/.claude/.config.json if it exists, otherwise ~/.claude.json)
func PreTrustWorktree(worktreeDir string) error {
	absPath, err := filepath.Abs(worktreeDir)
	if err != nil {
		return fmt.Errorf("resolving worktree path: %w", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("finding home dir: %w", err)
	}

	// Step 1: Create sessions-index.json for the project directory.
	encoded := strings.NewReplacer(string(filepath.Separator), "-", ".", "-").Replace(absPath)
	projectDir := filepath.Join(homeDir, ".claude", "projects", encoded)

	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return fmt.Errorf("creating project dir: %w", err)
	}

	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
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
	}

	// Step 2: Set hasTrustDialogAccepted in the Claude Code config file.
	if err := setTrustInClaudeConfig(homeDir, absPath); err != nil {
		return fmt.Errorf("setting trust in claude config: %w", err)
	}

	return nil
}

// setTrustInClaudeConfig reads the Claude Code config file, sets
// projects[absPath].hasTrustDialogAccepted = true, and writes it back.
func setTrustInClaudeConfig(homeDir, absPath string) error {
	// Resolve config file path: prefer ~/.claude/.config.json if it exists,
	// otherwise use ~/.claude.json.
	configPath := filepath.Join(homeDir, ".claude", ".config.json")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		configPath = filepath.Join(homeDir, ".claude.json")
	}

	// Read existing config or start fresh.
	var config map[string]any
	data, err := os.ReadFile(configPath)
	if err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			config = make(map[string]any)
		}
	} else {
		config = make(map[string]any)
	}

	// Ensure projects map exists.
	projects, ok := config["projects"].(map[string]any)
	if !ok {
		projects = make(map[string]any)
		config["projects"] = projects
	}

	// Get or create the project entry, preserving existing fields.
	entry, ok := projects[absPath].(map[string]any)
	if !ok {
		entry = make(map[string]any)
		projects[absPath] = entry
	}

	// Check if already trusted to avoid unnecessary writes.
	if trusted, ok := entry["hasTrustDialogAccepted"].(bool); ok && trusted {
		return nil
	}

	entry["hasTrustDialogAccepted"] = true

	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling claude config: %w", err)
	}

	// Ensure parent directory exists (for ~/.claude.json the parent is ~,
	// for ~/.claude/.config.json we need ~/.claude/).
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	if err := os.WriteFile(configPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing claude config: %w", err)
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

# klaus

Multi-agent orchestrator for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). Start a normal Claude Code session that can fan out work to parallel autonomous agents, each in its own git worktree and tmux pane.

## Quick start

```bash
klaus
```

That's it. Run it from a repo or from your home directory — either works. You're in an interactive Claude Code session. Talk to Claude normally — plan features, debug issues, review code. When there's work that can run in parallel, Claude (or you) spawns agents:

```
You: We need to fix the flaky auth test, add dark mode to settings,
     and update the API docs. Can you launch agents for each?

Claude: [runs klaus launch for each task]
```

Three new tmux panes appear. Each agent works independently in its own worktree, pushes a branch, and opens a PR. Your coordinator session stays focused on the big picture.

## What happens when you run `klaus`

1. **In a repo:** a fresh git worktree is created from `origin/main`. **Anywhere else:** a scratch workspace under `~/.klaus/sessions/`
2. Claude Code starts interactively in that workspace
3. You talk to Claude as usual — it has `klaus` on PATH
4. When Claude runs `klaus launch`, a new tmux pane splits off with an autonomous agent
5. `klaus status` gives you and Claude a dashboard of all running agents
6. When you're done, `klaus cleanup --all` tears everything down

Klaus is repo-agnostic. You can run it from your home directory and target any repo with `klaus launch --repo owner/repo` or set a session default with `klaus target owner/repo`.

The session is the experience. The other commands are infrastructure.

## Install

### Nix flake (recommended)

Add as a flake input to your repo:

```nix
inputs.klaus = {
  url = "github:patflynn/klaus";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

Then add `klaus.packages.${system}.default` to your devShell's `buildInputs`.

### From source

```bash
git clone https://github.com/patflynn/klaus.git
cd klaus
go build -o ~/.local/bin/klaus ./cmd/klaus/
```

## Commands

The coordinator session uses these — you generally don't run them directly:

| Command | Purpose |
|---------|---------|
| `klaus session` | Start an interactive coordinator session |
| `klaus launch "<prompt>"` | Spawn an autonomous agent |
| `klaus launch --repo owner/repo "<prompt>"` | Launch an agent against a different GitHub repo |
| `klaus target owner/repo` | Set session-level default target repo |
| `klaus watch <pr-number>` | Monitor CI for a PR and fix failures autonomously |
| `klaus status` | Dashboard of all runs (with CI, conflict, and merge-readiness columns) |
| `klaus logs <id>` | View agent output (live, replay, or raw) |
| `klaus cleanup <id>\|--all` | Tear down worktrees, panes, and state |
| `klaus push-log <id>` | Force-push a log held back for sensitivity |
| `klaus project add <owner/repo>` | Register a project (clones if needed) |
| `klaus project list` | Show registered projects |
| `klaus project remove <name>` | Unregister a project |
| `klaus project set-dir <path>` | Set the default projects directory |
| `klaus new <project-name>` | Scaffold a new project using principles-based generation |
| `klaus dashboard` | Live TUI dashboard for monitoring agents and PRs |
| `klaus merge <pr>...` | Sequentially merge PRs with conflict resolution |
| `klaus init` | Scaffold `.klaus/` config (optional, for customization) |

### `klaus watch`

Monitor CI checks for an existing PR. When a check fails, the watch agent reads the failure logs, diagnoses the issue, pushes a fix, and repeats until all checks pass. It also handles merge conflicts and addresses review comments from trusted reviewers.

```bash
klaus watch 42
```

### `klaus launch --repo`

Launch an agent against a different GitHub repository. The repo is cloned (or fetched if already cached) and the agent gets its own worktree in that clone. State is still tracked in the host repo.

```bash
klaus launch --repo owner/repo "Fix the bug in their API"
```

### `klaus target`

Set a session-level default target repo. When the coordinator session is not inside a git repo, this avoids needing `--repo` on every `klaus launch`.

```bash
klaus target owner/repo              # set default
klaus target                         # show current target
klaus target --clear                 # remove default
```

The targeting priority for `klaus launch` is:
1. `--repo` flag (explicit, always wins)
2. Current git repo (if in one)
3. Session target (`klaus target` setting)
4. Error with usage hint

### `klaus status` columns

The status dashboard shows these columns for each run:

| Column | Values | Meaning |
|--------|--------|---------|
| CI | `passing` / `failing` / `pending` / `unknown` | CI check status for the PR |
| CONFLICTS | `none` / `yes` / `unknown` | Whether the PR has merge conflicts |
| MERGE | `ready` / `blocked` / `pending` | Overall merge readiness (combines CI, conflicts, and review status) |

### `klaus dashboard`

Live TUI that monitors all active agents and their PR statuses. Groups runs by repository, auto-refreshes via filesystem watching for local state and GitHub polling every 30s.

Keyboard shortcuts: `q` quit, `r` force refresh.

### `klaus merge`

Sequentially merges a list of PRs. Handles conflicts by rebasing onto main, verifying the build, and force-pushing. Waits for CI to pass before merging (up to 10 min).

```bash
klaus merge 42 43 44
klaus merge --dry-run 42
klaus merge --merge-method rebase --no-delete-branch 42
```

Flags: `--dry-run`, `--merge-method` (squash/merge/rebase), `--no-delete-branch`.

### `klaus project`

Manage a persistent registry of projects. The registry maps short names to local paths and is stored in `~/.klaus/projects.json`.

```bash
klaus project add owner/repo              # clone into projects dir and register
klaus project add owner/repo --path .     # register an existing local checkout
klaus project add my-tool                 # search your GitHub repos by name
klaus project list                        # show all registered projects
klaus project remove my-tool              # unregister (does not delete the clone)
klaus project set-dir ~/hack              # set the default clone directory
```

### `klaus new`

Creates a new GitHub repo and launches a Claude agent to scaffold it. Reads principles from `.klaus/principles.md` (or built-in defaults). The agent makes all scaffolding decisions based on those principles — no templates.

```bash
klaus new my-project
```

## Configuration

Klaus works out of the box with sensible defaults. To customize, run `klaus init` to scaffold a `.klaus/` directory (or `~/.klaus/config.json` when outside a repo), or create the files yourself. Configuration layers: defaults → `~/.klaus/config.json` → `.klaus/config.json`.

**`.klaus/config.json`** — Override defaults:
```json
{
  "worktree_base": "/tmp/klaus-sessions",
  "default_budget": "5.00",
  "data_ref": "refs/klaus/data",
  "default_branch": "main",
  "trusted_reviewers": ["gemini-code-assist[bot]"]
}
```

**`.klaus/prompt.md`** — Custom system prompt for launched agents. Go template variables: `{{.RunID}}`, `{{.Issue}}`, `{{.Branch}}`, `{{.RepoName}}`. Customize this to match your repo's conventions, test commands, and PR workflow.

**`.klaus/session-prompt.md`** — Custom prompt for the coordinator session. Same template variables.

**`.klaus/watch-prompt.md`** — Custom prompt for the watch agent. Additional variable: `{{.PR}}`.

## Under the hood

- **Worktrees** isolate each agent — they can't step on each other or your working tree
- **tmux panes** give live visibility into each agent's progress
- **JSONL logs** are saved for replay and post-run analysis
- **Sensitivity scanning** checks logs for private IPs, SSH keys, and credentials before persisting
- **State storage** — session state lives in `~/.klaus/sessions/` (ephemeral, machine-local), while finalized run artifacts sync to the repo's data ref
- **Data ref** (`refs/klaus/data`) stores run metadata without polluting your branch list

## Requirements

- `tmux` (sessions run inside tmux)
- `claude` (Claude Code CLI)
- `git` (needed for agent worktrees; sessions can run without it)
- `gh` (GitHub CLI, for PR operations)

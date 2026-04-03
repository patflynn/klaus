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
5. Agents push branches and open PRs — `klaus dashboard` picks them up automatically
6. The pipeline monitors CI, dispatches fix agents on failure, and auto-merges when approved
7. Your job shifts from babysitting agents to reviewing and approving PRs
8. When you're done, `klaus cleanup --all` tears everything down

Klaus is repo-agnostic. You can run it from your home directory and target any repo with `klaus launch --repo owner/repo` or set a session default with `klaus target owner/repo`.

The session is the experience. The pipeline handles the rest.

## The PR pipeline

Once an agent opens a PR, the dashboard's event-driven pipeline takes over:

```
PR created → CI pending → CI passed → Approved → Merged
                ↓                        ↓
            CI failed               Conflicts?
                ↓                        ↓
           Fix agent              Rebase agent
```

- **CI fails** — a fix agent is dispatched automatically (via `--pr`) to push a correction
- **Review comments** — an agent is dispatched to address requested changes
- **Approved + CI green + no conflicts** — auto-merge (when `auto_merge_on_approval` is enabled)
- **Merge conflicts** — a rebase agent resolves them before merging

Pipeline stages per PR: `ci_pending` → `ci_passed` → `approved` → `merged`, with failure paths back through `ci_failed` or `changes_requested`.

You can also drive the pipeline manually with `klaus approve` and `klaus merge`.

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
| `klaus launch --repo <project-name> "<prompt>"` | Launch an agent using a registered project |
| `klaus launch --pr <number> "<prompt>"` | Push fixes to an existing PR's branch |
| `klaus target owner/repo` | Set session-level default target repo |
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
| `klaus approve <pr>...` | Approve PRs for merging |
| `klaus merge <pr>...` | Sequentially merge PRs with conflict resolution |
| `klaus init` | Scaffold `.klaus/` config (optional, for customization) |

### `klaus launch --pr`

Push fixes to an existing PR's branch instead of creating a new PR. The agent checks out the PR's branch, makes changes, and pushes directly — the PR updates automatically. Useful for addressing review comments or fixing CI failures on an existing PR.

```bash
klaus launch --pr 42 "Address the review comments"
klaus launch --pr 42 --issue 10 "Fix the auth bug mentioned in review"
```

The `--pr` and `--issue` flags can coexist (the agent may reference the issue in commits).

### `klaus launch --repo`

Launch an agent against a different GitHub repository. The repo is cloned (or fetched if already cached) and the agent gets its own worktree in that clone. State is still tracked in the host repo.

```bash
klaus launch --repo owner/repo "Fix the bug in their API"
```

### Sandbox (remote execution)

When `sandbox_host` is set in `~/.klaus/config.json`, agents run remotely via SSH on the sandbox host instead of locally. The worktree is synced to the sandbox before launch, and results are synced back after completion. Log streaming, formatting, and finalization still happen locally.

```json
{"sandbox_host": "klaus-worker-0"}
```

If the sandbox is unreachable, execution falls back to local automatically. Use `--local` to force local execution, or `--host <name>` to override the configured sandbox host.

```bash
klaus launch --local "Run this locally"
klaus launch --host my-sandbox "Run on a specific host"
```

The dashboard shows `[sandbox]` tags on remotely-executed agents and displays sandbox reachability status in the header. The `status` command includes a HOST column.

### `klaus target`

Set a session-level default target repo. When the coordinator session is not inside a git repo, this avoids needing `--repo` on every `klaus launch`. Accepts a registered project name or `owner/repo`.

```bash
klaus target owner/repo              # set default by owner/repo
klaus target my-project              # set default using registered project name
klaus target                         # show current target
klaus target --clear                 # remove default
```

The targeting priority for `klaus launch` is:
1. `--repo` flag — if it matches a registered project name (no `owner/` prefix), resolves to that project's local path
2. `--repo` flag — `owner/repo` or full URL (clones/fetches from GitHub)
3. Current git repo (if in one)
4. Session target (`klaus target` setting)
5. Error with usage hint

### `klaus status` columns

The status dashboard shows these columns for each run:

| Column | Values | Meaning |
|--------|--------|---------|
| CI | `passing` / `failing` / `pending` / `unknown` | CI check status for the PR |
| CONFLICTS | `none` / `yes` / `unknown` | Whether the PR has merge conflicts |
| MERGE | `ready` / `blocked` / `pending` | Overall merge readiness (combines CI, conflicts, and review status) |

### `klaus dashboard`

Live TUI view of the PR pipeline. Groups runs by repository, auto-refreshes via filesystem watching and GitHub polling every 30s. Keyboard shortcuts: `q` quit, `r` force refresh.

### `klaus approve`

Mark PRs as approved for merging. By default, `klaus merge` requires approval before merging.

```bash
klaus approve 42 43                  # approve specific PRs
klaus approve --all                  # approve all merge-ready PRs
klaus approve --run 20260328-1603-a3f2  # approve by run ID
```

### `klaus merge`

Sequentially merges a list of PRs. Handles conflicts by rebasing onto main, verifying the build, and force-pushing. Waits for CI to pass before merging (up to 10 min).

By default, PRs must be approved with `klaus approve` before merging. Unapproved PRs trigger an interactive prompt (or are skipped with `--yes`).

```bash
klaus merge 42 43 44
klaus merge --dry-run 42
klaus merge --merge-method rebase --no-delete-branch 42
klaus merge --force 42               # bypass approval check
klaus merge --yes 42 43              # skip unapproved PRs without prompting
```

Flags: `--dry-run`, `--merge-method` (squash/merge/rebase), `--no-delete-branch`, `--force` (bypass approval), `--yes` (skip unapproved).

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

Creates a new GitHub repo and launches a Claude agent to scaffold it. Reads principles from `.klaus/principles.md` (or built-in defaults). The agent makes all scaffolding decisions based on those principles — no templates. The new project is automatically registered in the project registry.

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
  "trusted_reviewers": ["gemini-code-assist[bot]"],
  "require_approval": true,
  "auto_merge_on_approval": false
}
```

**`.klaus/prompt.md`** — Custom system prompt for launched agents. Go template variables: `{{.RunID}}`, `{{.Issue}}`, `{{.Branch}}`, `{{.RepoName}}`. Customize this to match your repo's conventions, test commands, and PR workflow.

**`.klaus/session-prompt.md`** — Custom prompt for the coordinator session. Same template variables.

**`.klaus/pr-fix-prompt.md`** — Custom prompt for PR-fix agents. Additional variable: `{{.PR}}`.

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

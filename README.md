# klaus

Multi-agent orchestrator for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). Start a normal Claude Code session that can fan out work to parallel autonomous agents, each in its own git worktree and tmux pane.

## Quick start

```bash
klaus
```

That's it. You're in an interactive Claude Code session in a clean worktree. Talk to Claude normally — plan features, debug issues, review code. When there's work that can run in parallel, Claude (or you) spawns agents:

```
You: We need to fix the flaky auth test, add dark mode to settings,
     and update the API docs. Can you launch agents for each?

Claude: [runs klaus launch for each task]
```

Three new tmux panes appear. Each agent works independently in its own worktree, pushes a branch, and opens a PR. Your coordinator session stays focused on the big picture.

## What happens when you run `klaus session`

1. A fresh git worktree is created from `origin/main`
2. Claude Code starts interactively in that worktree
3. You talk to Claude as usual — it has `klaus` on PATH
4. When Claude runs `klaus launch`, a new tmux pane splits off with an autonomous agent
5. `klaus status` gives you and Claude a dashboard of all running agents
6. When you're done, `klaus cleanup --all` tears everything down

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
| `klaus watch <pr-number>` | Monitor CI for a PR and fix failures autonomously |
| `klaus status` | Dashboard of all runs (with CI, conflict, and merge-readiness columns) |
| `klaus logs <id>` | View agent output (live, replay, or raw) |
| `klaus cleanup <id>\|--all` | Tear down worktrees, panes, and state |
| `klaus push-log <id>` | Force-push a log held back for sensitivity |
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

### `klaus status` columns

The status dashboard shows these columns for each run:

| Column | Values | Meaning |
|--------|--------|---------|
| CI | `passing` / `failing` / `pending` / `unknown` | CI check status for the PR |
| CONFLICTS | `none` / `yes` / `unknown` | Whether the PR has merge conflicts |
| MERGE | `ready` / `blocked` / `pending` | Overall merge readiness (combines CI, conflicts, and review status) |

## Configuration

Klaus works out of the box with sensible defaults. To customize, run `klaus init` to scaffold a `.klaus/` directory, or create the files yourself:

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
- **Data ref** (`refs/klaus/data`) stores run metadata without polluting your branch list

## Requirements

- `tmux` (sessions run inside tmux)
- `claude` (Claude Code CLI)
- `git`
- `gh` (GitHub CLI, for PR operations)

# klaus

Multi-agent orchestrator for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). Runs parallel autonomous agents in isolated git worktrees, each in its own tmux pane.

## Quick start

```bash
# Add to your repo
klaus init

# Start a coordinator session (interactive Claude Code in a clean worktree)
klaus session

# Launch an autonomous agent
klaus launch "Add rate limiting to the API" --issue 42 --budget 3

# Check what's running
klaus status

# View agent output
klaus logs 20260212-1430-a3f2

# Clean up when done
klaus cleanup --all
```

## What it does

You're in a tmux session working on your repo. You have three things to do. Instead of doing them sequentially, you launch three agents:

```bash
klaus launch "Fix the flaky test in auth_test.go" --issue 10
klaus launch "Add dark mode support to the settings page" --issue 15
klaus launch "Update the API docs for the new endpoints" --issue 22
```

Each agent gets:
- Its own git worktree branched from `origin/main`
- Its own tmux pane with live-formatted output
- A JSONL log for replay
- A state file tracking cost, duration, and PR URL

You keep working. Agents push branches and open PRs. `klaus status` shows you the dashboard. When they're done, review the PRs and `klaus cleanup --all`.

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

| Command | What it does |
|---------|-------------|
| `klaus init` | Scaffold `.klaus/` config directory |
| `klaus launch "<prompt>"` | Launch an autonomous agent in a new tmux pane |
| `klaus session` | Start an interactive coordinator session |
| `klaus status` | Show all runs and their state |
| `klaus logs <id>` | View agent output (live, replay, or raw) |
| `klaus cleanup <id>\|--all` | Remove worktrees, panes, and state |
| `klaus push-log <id>` | Force-push a log that was held back for sensitivity |

## Configuration

`klaus init` creates `.klaus/` with two files:

**`.klaus/config.json`** — Settings:
```json
{
  "worktree_base": "/tmp/klaus",
  "default_budget": "5.00",
  "data_ref": "refs/klaus/data",
  "default_branch": "main"
}
```

**`.klaus/prompt.md`** — System prompt template. Supports Go template variables: `{{.RunID}}`, `{{.Issue}}`, `{{.Branch}}`, `{{.RepoName}}`. Customize this for your repo's conventions.

## How it works

- **Worktrees** isolate each agent so they can't step on each other or your working tree
- **tmux panes** give you live visibility into each agent's progress
- **JSONL logs** are saved for replay and analysis
- **Sensitivity scanning** checks logs for private IPs, SSH keys, and credentials before persisting to git
- **Data ref** (`refs/klaus/data`) stores run metadata on a custom git ref that doesn't pollute your branch list

## Requirements

- `tmux` (must be running inside a tmux session)
- `claude` (Claude Code CLI)
- `git`
- `gh` (GitHub CLI, for PR operations)

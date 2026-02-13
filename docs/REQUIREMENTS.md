# Klaus: Functional Requirements

## Overview

Klaus is a multi-agent orchestrator for Claude Code. It manages parallel autonomous Claude Code sessions, each running in an isolated git worktree with its own tmux pane.

## Core Requirements

### Agent Launch (`klaus launch`)
- Accept a natural-language prompt describing the task
- Create an isolated git worktree branched from `origin/main`
- Launch Claude Code in autonomous mode (`--dangerously-skip-permissions`) in a new tmux pane
- Stream output through a JSONL formatter for human-readable progress
- Record run state (ID, prompt, branch, worktree path, tmux pane, budget, timestamps)
- Support `--issue N` to reference a GitHub issue
- Support `--budget N` to set max spend (default: $5.00)
- Must be run inside a tmux session

### Interactive Session (`klaus session`)
- Create a coordinator worktree for interactive Claude Code use
- Run Claude Code in interactive mode (user approves actions)
- Allow launching sub-agents from within the session

### Status Tracking (`klaus status`)
- Display a table of all runs with: ID, status, cost, issue, PR, prompt
- Detect live status by checking tmux pane existence
- Show cost from finalized logs or budget estimate

### Log Viewing (`klaus logs`)
- `--live`: Show live tmux pane output (default)
- `--replay`: Re-format saved JSONL log through the stream formatter
- `--raw`: Dump raw JSONL log file

### Cleanup (`klaus cleanup`)
- Kill tmux pane if still alive
- Remove git worktree
- Delete local branch (delete remote branch if PR was merged)
- Remove state file
- Support `--all` to clean up everything

### Sensitive Data Protection
- Scan JSONL logs before persisting to git
- Detect: private IPs, SSH keys, credential patterns, secret file references
- Skip log push if sensitive data found, warn user
- `klaus push-log` to force-push after manual review

### Data Persistence
- Store run state in `.git/klaus/runs/`
- Store logs in `.git/klaus/logs/`
- Sync completed runs to `refs/klaus/data` (custom git ref, not a branch)
- Push data ref to remote

### Configuration (`klaus init`)
- Scaffold `.klaus/` directory with default config
- `.klaus/config.json` for settings (worktree base path, default budget, etc.)
- `.klaus/prompt.md` for system prompt template with Go template variables

## Non-Functional Requirements
- Single binary, minimal dependencies
- Works with any git repository (not repo-specific)
- Fast startup
- No network calls unless explicitly needed (git push, gh commands)

# Klaus: Architecture & Design

## Package Architecture

```
cmd/klaus/main.go          → Entry point, calls cmd.Execute()
internal/cmd/              → Cobra command definitions
internal/run/              → Run state management (pure logic)
internal/stream/           → JSONL stream formatting (pure logic)
internal/scan/             → Sensitivity scanning (pure logic)
internal/git/              → Git worktree and data ref operations
internal/tmux/             → Tmux pane management
internal/config/           → Configuration loading and defaults
```

## Data Flow

### Agent Launch
```
User prompt
  → klaus launch "<prompt>" --issue N --budget N
  → Generate run ID (YYYYMMDD-HHMM-XXXX)
  → git fetch origin main
  → git worktree add /tmp/klaus-sessions/<id> -b agent/<id> origin/main
  → Write state file to .git/klaus/runs/<id>.json
  → Build claude command with stream-json output
  → tmux split-window: claude | tee <log> | klaus _format-stream; klaus _finalize <id>
```

### Stream Processing
```
Claude JSONL output
  → Pipe through tee (save raw log)
  → Pipe through _format-stream (display progress)
  → On exit: _finalize extracts cost/duration/PR URL, updates state
```

### Data Persistence
```
State + Log files (local .git/klaus/)
  → Sensitivity scan on log
  → If clean: commit both to refs/klaus/data using temp index
  → If sensitive: commit state only, warn user
  → Push refs/klaus/data to remote
```

## State File Format

```json
{
  "id": "20260210-1430-a3f2",
  "prompt": "Add bluetooth config",
  "issue": "42",
  "branch": "agent/20260210-1430-a3f2",
  "worktree": "/tmp/klaus-sessions/20260210-1430-a3f2",
  "tmux_pane": "%5",
  "budget": "5.00",
  "log_file": "/path/to/.git/klaus/logs/20260210-1430-a3f2.jsonl",
  "created_at": "2026-02-10T14:30:00-08:00",
  "cost_usd": 3.42,
  "duration_ms": 45000,
  "pr_url": "https://github.com/org/repo/pull/123"
}
```

## Configuration

`.klaus/config.json`:
```json
{
  "worktree_base": "/tmp/klaus-sessions",
  "default_budget": "5.00",
  "data_ref": "refs/klaus/data",
  "default_branch": "main"
}
```

`.klaus/prompt.md`: Go template with variables:
- `{{.RunID}}` — The run ID
- `{{.Issue}}` — GitHub issue number (if provided)
- `{{.Branch}}` — The agent's branch name
- `{{.RepoName}}` — Repository name

## Design Decisions

1. **Single cobra dependency**: Keeps the binary small and supply chain minimal.
2. **JSON config (not YAML)**: Uses stdlib `encoding/json`, no extra dependency.
3. **Custom git ref**: `refs/klaus/data` doesn't appear as a branch in GitHub UI.
4. **Temp index for commits**: Git plumbing operations avoid touching the working tree.
5. **Sensitivity scanning**: Regex-based, runs before any log is persisted to git.
6. **Generic system prompt**: Template-based, works with any repo (not hardcoded).

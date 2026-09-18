# klaus

[![Build Status](https://github.com/patflynn/klaus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/patflynn/klaus/actions/workflows/ci.yml)

Multi-agent orchestrator for Claude Code, Codex, and Antigravity CLI (`agy`). Start an interactive coordinator session that can fan out work to parallel autonomous agents, each in its own git worktree and its own tmux pane in a detached session — off your screen, but still yours to inspect.

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

Three agents start in the background. Each works independently in its own worktree, pushes a branch, and opens a PR. Your window is untouched — the agents' tmux panes live in a separate detached session, and you watch them through the dashboard, `klaus status`, and `klaus logs`.

## Choosing backends

Choose the coordinator and workers independently. Existing configuration and
sessions continue to use Claude unless you select another backend:

```bash
klaus new --backend codex --agent-backend agy
klaus new --backend agy --agent-backend codex
klaus launch --backend codex "Fix the failing tests"
klaus launch --backend agy --effort high "Implement the settings page"
```

`klaus`, `klaus session`, and `klaus new` accept `--backend` for the coordinator
and `--agent-backend` for the session's worker default. `klaus launch` and
`klaus scaffold` accept `--backend` for that worker. Automatic CI/review fix
agents use the saved session worker default too. `klaus status` includes a
BACKEND column.

Set persistent defaults in `~/.klaus/config.json` or `.klaus/config.json`:

```json
{
  "default_coordinator_backend": "codex",
  "default_agent_backend": "agy",
  "backends": {
    "codex": { "effort": "high" },
    "agy": { "effort": "medium" }
  }
}
```

Each entry in `backends` can set `model`, `effort`, `review_model`, and
`review_backend` (see [Cross-model review](#cross-model-review)). Model
identifiers are passed to that CLI unchanged. Legacy `default_agent_model`,
`default_agent_effort`, and `pre_review.review_model` remain Claude defaults;
they never send a Claude model name to Codex or agy. Explicit worker `--model`
and `--effort` flags override defaults. A per-backend entry replaces the legacy
model/effort defaults for that backend, including when its fields are empty.

Worker backend precedence is `--backend`, saved session selection,
`KLAUS_AGENT_BACKEND`, configuration, then Claude. Backend selection is stored
on each run. Resuming a coordinator keeps its saved backend; use `klaus new`
to switch to a different coordinator backend.

| Capability | Claude | Codex | agy |
|---|---|---|---|
| Interactive coordinator, headless workers, local/SSH execution | Yes | Yes | Yes |
| Formatted logs, PR detection, failure reporting | Yes | Yes | Yes |
| Coordinator resume | Saved conversation ID | Most recent conversation in the session workspace | Workspace-specific conversation ID |
| Worker `--resume-from` | Fork with staged transcript | Fork a recorded local Codex thread on the same machine | Fresh conversation, with a warning |
| Dollar budget / budget pause | Yes | Unavailable | Unavailable |
| Stored trajectory replay (`--replay`) | Yes | Unavailable | Unavailable |
| Shared coordinator memory | Claude memory directory | CLI's own memory behavior | CLI's own memory behavior |

For Codex and agy, explicit `--budget` and `--replay` are rejected; the legacy
`default_budget` does not apply, and unknown cost is displayed as unavailable.
agy workers use a 24-hour print timeout instead of its short CLI default.
`--resume-from` never transfers conversations between backends or machines;
when a compatible conversation is unavailable, the worker starts fresh. Use
`--pr` to continue the actual branch changes. agy coordinator IDs are recovered
from its workspace cache; missing cache metadata starts a fresh conversation
with a warning rather than attaching to an unrelated conversation. A resumed
agy coordinator receives a short continuation prompt instead of repeating the
original instructions.

Pre-PR peer review prefers a different family from the worker's (see below)
and falls back to the worker's own backend, so Codex and agy workers do not
require Claude to be installed. Authenticate each selected CLI beforehand and
make it available on PATH on the machine that executes it (including any
`sandbox_host`). Workers retain Klaus's existing unattended permission policy;
Codex uses its explicit approval/sandbox bypass, and agy uses its permission
bypass. Run untrusted tasks on an appropriately isolated worker host.

### Cross-model review

Same-family review shares blind spots, so klaus reviews code with a different
model family from the one that wrote it:

```json
{
  "cross_review": { "enabled": true, "order": ["codex", "claude", "agy"], "post": true, "max_rounds": 2 },
  "backends": { "claude": { "review_backend": "codex" } }
}
```

The reviewer family is `backends.<author>.review_backend` if set, else the first
`cross_review.order` entry that is not the author and whose CLI is on PATH, else
the author's own family. `enabled: false` skips the order and keeps the author's
family. The model is `backends.<reviewer>.review_model`, else
`pre_review.review_model` for Claude (default `haiku`), else the CLI default.
The same choice drives the `_pre-review` agents run before opening a PR; it
prints the reviewer it used.

`klaus review <pr>` gets a second opinion on an open PR:

```bash
klaus review 303                                  # reviewer chosen as above
klaus review 303 --repo owner/repo --backend agy  # explicit reviewer
klaus review 303 --post                           # submit it to the PR
```

The PR's author family comes from the klaus run that opened it (Claude if none).
klaus refuses a same-family review unless you pass `--backend` together with
`--allow-same-family`. The reviewer sees only `gh pr diff` plus the PR title and
description, runs read-only in an empty temp directory, and returns findings and
a short verdict on whether the change matches its stated intent.

`--post` (default: `cross_review.post`) submits one GitHub review with event
`COMMENT`: findings on lines in the diff become inline comments, the rest go in
the body next to the verdict. **Cross-model reviews are second opinions for the
operator, never approvals**; klaus never posts `APPROVE` or `REQUEST_CHANGES`,
and approval stays with `klaus approve`. Posted inline findings feed the normal
trusted-review fix loop (see [docs/PIPELINE.md](docs/PIPELINE.md#4-review--approval)).
Posting is refused when the same reviewer already reviewed the PR's head
commit, or when the PR already has `max_rounds` cross-reviews.

The coordinator prompt describes `klaus watch`. Claude can attach its Monitor
tool; other backends use their own background tools or `klaus status` and
`klaus logs`. Klaus does not emulate Claude's Monitor tool.

CLI event contracts: [Codex non-interactive mode](https://developers.openai.com/codex/noninteractive)
and [Antigravity headless mode](https://antigravity.google/docs/cli/headless).

## What happens when you run `klaus`

1. **In a repo:** a fresh git worktree is created from `origin/main`. **Anywhere else:** a scratch workspace under `~/.klaus/sessions/`
2. The selected backend starts interactively in that workspace
3. You talk to Claude as usual — it has `klaus` on PATH
4. When Claude runs `klaus launch`, an autonomous agent starts in a detached tmux session — off your screen, but still a real process you can inspect and kill
5. Agents push branches and open PRs — `klaus dashboard` picks them up automatically
6. The pipeline monitors CI, dispatches fix agents on failure, and auto-merges when approved
7. Your job shifts from babysitting agents to reviewing and approving PRs
8. When you're done, `klaus cleanup --all` tears everything down

Klaus is repo-agnostic. You can run it from your home directory and target any repo with `klaus launch --repo owner/repo` or set a session default with `klaus target owner/repo`.

The coordinator's Claude Code persistent memory is shared across sessions: each session's project memory directory (`~/.claude/projects/<encoded-path>/memory`) is symlinked to `~/.klaus/memory`, so what the coordinator learns in one session carries over to the next. Existing memory files from older sessions are migrated into the shared directory when a session is resumed. Launched agents don't share this memory — each agent starts clean.

The session is the experience. The pipeline handles the rest.

## The PR pipeline

Once an agent opens a PR, the dashboard's event-driven pipeline takes over:

```
PR created → CI pending → CI passed → Approved → Merged
                ↓             ↓          ↓
            CI failed  Changes Req.   Conflicts?
                ↓             ↓          ↓
           Fix agent      Fix agent   Rebase agent
```

- **CI fails** — a fix agent is dispatched automatically (via `--pr`) to push a correction
- **Review comments** — an agent is dispatched to address requested changes
- **Approved + CI green + no conflicts** — auto-merge (when `auto_merge_on_approval` is enabled)
- **Behind the base branch** — the branch is updated from base first, then merged once CI re-runs
- **Merge conflicts** — a rebase agent resolves them before merging

When a `pr-fix` run is already active on a PR — including coordinator-launched runs from `klaus launch --pr` — the pipeline will not auto-dispatch additional fix or rebase agents on it. This prevents races during multi-step refactors where intermediate commits may fail CI before the run completes.

Pipeline stages per PR: `ci_pending` → `ci_passed` → `approved` → `merged`, with failure paths back through `ci_failed` or `changes_requested`. Budget-exhausted agents land in `budget_paused` — see [Budget pause and resume](#budget-pause-and-resume) below. A PR whose merge or branch update keeps failing is retried once a minute for three attempts, then moves to `stalled` and emits a single `pipeline:stalled` event for a human to pick up.

You can also drive the pipeline manually with `klaus approve` and `klaus merge`.

### Real-time event channel

Pipeline events (PR created/approved/merged, CI passed/failed, agent errors) are appended to `~/.klaus/sessions/$KLAUS_SESSION_ID/events.jsonl` and exposed as a streaming channel via `klaus watch`. It's designed for [Claude Code's Monitor tool](https://docs.anthropic.com/en/docs/claude-code) — each matching event becomes a notification injected into the coordinator's context between turns, so the coordinator can react to merges or CI failures without you having to mention them.

```bash
klaus watch                          # default filter, follow new events
klaus watch --since-start            # replay everything in the file, then follow
klaus watch --filter pr:merged       # narrow to a single event type (repeatable)
klaus watch --filter-out pr:approved # subtract from the default filter
klaus watch --filter pipeline:stalled # only PRs the pipeline gave up on
klaus watch --json                   # raw JSONL passthrough
klaus watch --list-types             # show live and reserved event types
```

The coordinator session's system prompt automatically suggests arming a persistent Monitor on `klaus watch` at startup. The dashboard reads the same `events.jsonl` file independently — both consumers can run side by side.

## Budget pause and resume

When an agent exhausts its `--budget` cap, klaus does not silently kill the worktree. Instead it parks the in-progress work on GitHub:

1. Commits any uncommitted changes in the worktree (`WIP from klaus run <id> (budget paused)`).
2. Pushes the branch to `origin` (with `--force-with-lease`).
3. Opens a draft PR — or updates the existing one — and applies the `klaus:budget-paused` label.
4. Posts a one-line PR comment explaining the pause and how to continue.
5. Cleans up the worktree and tmux pane.

Claude structured result events remain authoritative when the process exits
nonzero after reaching its budget or turn limit; those stops remain resumable.

The draft PR + label is the persisted state — klaus does not keep an in-process "paused" status. The dashboard surfaces these as **budget paused, awaiting decision** via the pipeline FSM (which reads the label off GitHub).

To resume, dispatch a fresh agent against the PR's branch:

```bash
klaus launch --pr <num> "continue the work"
```

The new agent sees the WIP commit and picks up from there. When the follow-up agent's `_finalize` runs, the `klaus:budget-paused` label is automatically removed and an `agent:resumed` event is emitted. If the follow-up agent *also* exhausts its budget, the cycle repeats (a new WIP commit, label re-applied).

### Trajectory replay

By default, `klaus launch --pr` against a budget-paused PR does more than pick up the WIP commit: it **continues the previous agent's Claude conversation** instead of starting one cold. At finalize time klaus stores the resume-able conversation trajectory (the JSONL `claude` itself writes under `~/.claude/projects/…`) on `refs/klaus/data` at `sessions/<run-id>.jsonl`. On resume it fetches that blob, restores it into the new worktree's project dir, and invokes `claude --resume <uuid>`. The conversation continues from the exact point of pause — no re-grepping or re-orienting — which is faster and cheaper than a fresh agent.

Replay is best-effort and **falls back to a fresh agent** when any of these hold:

- The trajectory was never pushed (skipped because `klaus` detected potentially sensitive content), or the PR pre-dates this feature.
- The trajectory is larger than the size threshold (default 300KB; replaying a very large trajectory can cost more than re-exploration). Configure with `replay_threshold_kb` in `~/.klaus/config.json` or `--replay-threshold-kb`.
- No Claude session UUID could be determined for the paused run.
- You pass `--no-replay`.

Flags:

```bash
klaus launch --pr 42 "continue"                       # replay if eligible (default)
klaus launch --pr 42 --no-replay "continue"           # force a fresh agent
klaus launch --pr 42 --replay "continue"              # force replay, bypassing the size threshold
klaus launch --pr 42 --replay-threshold-kb 500 "..."  # raise the per-launch threshold
```

**Staleness semantics — what you opt into with replay.** The restored conversation is a snapshot from when the previous agent paused, replayed into a *new* worktree at a *different* path:

- **`cwd` / path mismatch.** The trajectory's tool calls reference the original (now-deleted) worktree path; the resumed agent runs in a fresh worktree. `claude` re-anchors to its current working directory, so new file operations are correct, but the conversation history still mentions the old path.
- **Tool-result staleness.** Cached `Read`/grep results in the history reflect the files *as they were at pause time*. The branch head carries the WIP commit, so current contents may differ. This is the same staleness any long-lived `claude --resume` has; the agent is expected to re-read before relying on stale output. klaus adds no special mitigation beyond restoring the trajectory faithfully.
- **No prompt-cache benefit.** Resumes happen long after the 5-minute cache TTL, so the replayed history is re-read as fresh input tokens. The size threshold exists precisely to keep that cost below the cost of a fresh re-exploration.

> Note: replay relies on the conversation file being available where finalize runs. For agents executed on a remote `sandbox_host`, the conversation file lives on the sandbox and is not synced back, so those runs fall back to a fresh agent.

To abandon the work, close the draft PR. To redirect, push manual commits to its branch.

There is no subcommand named `klaus resume` or `klaus finalize` — resuming happens through flags on `klaus launch`, and `_finalize` handles the WIP commit automatically:

- `klaus launch --pr <num> "..."` resumes a budget-paused PR (WIP commit + trajectory replay, as above).
- `klaus launch --resume-from <run-id> "..."` continues any prior run's conversation, paused or not. The follow-up runs in a fresh worktree on its own branch (unless combined with `--pr`) and keeps everything the previous agent learned — use it to correct an agent instead of killing it and re-briefing a cold one. It starts fresh if the prior run crashed or its transcript can't be located.

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

## Development

```bash
make test       # unit tests (go test ./...)
make test-e2e   # end-to-end tests against a real, isolated tmux server
make vet        # go vet ./...
```

The e2e suite (`e2e/`, guarded by the `e2e` build tag so `go test ./...` is
unaffected) drives the real `klaus` binary against a private tmux server and
real git repositories, faking only `claude`/`gh`. It requires `tmux` on PATH.
See [`e2e/README.md`](e2e/README.md) for the design and how to add scenarios.

## Commands

The coordinator session uses these — you generally don't run them directly:

| Command | Purpose |
|---------|---------|
| `klaus session` | Start an interactive coordinator session |
| `klaus launch "<prompt>"` | Spawn an autonomous agent |
| `klaus launch --prompt-file <path>` | Spawn an agent with the prompt read from a file |
| `klaus launch --repo owner/repo "<prompt>"` | Launch an agent against a different GitHub repo |
| `klaus launch --repo <project-name> "<prompt>"` | Launch an agent using a registered project |
| `klaus launch --pr <number> "<prompt>"` | Push fixes to an existing PR's branch |
| `klaus launch --model <name> --effort <level> "<prompt>"` | Pick the model and reasoning effort for the agent |
| `klaus target owner/repo` | Set session-level default target repo |
| `klaus status` | Dashboard of all runs (with CI, conflict, and merge-readiness columns) |
| `klaus logs <id>` | View agent output (live, replay, or raw) |
| `klaus cleanup <id>\|--all` | Tear down worktrees, panes, and state |
| `klaus push-log <id>` | Force-push a log held back for sensitivity |
| `klaus project add <owner/repo>` | Register a project (clones if needed) |
| `klaus project list` | Show registered projects |
| `klaus project remove <name>` | Unregister a project |
| `klaus project describe <name> <desc>` | Set a one-line description (empty string clears it) |
| `klaus project set-dir <path>` | Set the default projects directory |
| `klaus sync` | Fetch and fast-forward every registered project |
| `klaus new <project-name>` | Scaffold a new project using principles-based generation |
| `klaus webhook check` | Check registered projects for GitHub webhook configuration |
| `klaus webhook setup [project]` | Create missing webhooks for registered projects |
| `klaus dashboard` | Live TUI dashboard for monitoring agents and PRs |
| `klaus watch` | Stream pipeline events line-by-line (designed for Claude Code's Monitor tool) |
| `klaus review <pr> [--post]` | Second-opinion review of a PR by a different model family |
| `klaus approve <pr>...` | Approve PRs for merging (operator task) |
| `klaus merge <pr>...` | Sequentially merge PRs with conflict resolution |
| `klaus init` | Scaffold `.klaus/` config (optional, for customization) |

### `klaus launch --pr`

Push fixes to an existing PR's branch instead of creating a new PR. The agent checks out the PR's branch, makes changes, and pushes directly — the PR updates automatically. Useful for addressing review comments or fixing CI failures on an existing PR.

```bash
klaus launch --pr 42 "Address the review comments"
klaus launch --pr 42 --issue 10 "Fix the auth bug mentioned in review"
```

The `--pr` and `--issue` flags can coexist (the agent may reference the issue in commits).

### `klaus launch --prompt-file`

Read the agent's prompt from a file instead of the positional argument. The file is read verbatim, so backticks, quotes, `$VAR`, and newlines survive intact:

```bash
klaus launch --prompt-file /tmp/task.md
klaus launch --prompt-file /tmp/task.md --issue 42 --budget 10
```

Prefer this for long or technical prompts. A prompt passed as a shell argument goes through the shell first — in zsh, backticks inside a double-quoted string are command substitution, so code spans are deleted or replaced by command output before klaus sees them, and the agent gets a briefing with holes exactly where the detail was.

The positional prompt and `--prompt-file` are mutually exclusive: passing both, or neither, is an error. Everything downstream — the stored prompt in run state, `--issue`/`--pr` composition, budget handling — behaves identically either way.

### `klaus launch --repo`

Launch an agent against a different GitHub repository. The repo is cloned (or fetched if already cached) and the agent gets its own worktree in that clone. State is still tracked in the host repo.

```bash
klaus launch --repo owner/repo "Fix the bug in their API"
```

### `klaus launch --model` / `--effort`

Pick the model and reasoning effort for the agent's `claude` run — e.g. a cheap mechanical task on a smaller model at low effort while a large build keeps the big one:

```bash
klaus launch --model claude-haiku-4-5 --effort low "Update the CHANGELOG for v1.2"
klaus launch --effort max "Redesign the pipeline state machine"
```

`--effort` must be one of `low`, `medium`, `high`, `xhigh`, `max`. The model is passed through verbatim — `claude` itself rejects unknown models. Per-repo defaults come from `default_agent_model` / `default_agent_effort` in `.klaus/config.json`, with the flag overriding config; agents dispatched by the pipeline (`klaus launch --pr` fix agents) pick up the config defaults too. When neither flag nor config sets a value, the `claude` command carries no `--model`/`--effort` at all, so claude's own resolution applies unchanged. The chosen values are recorded in the run state.

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
| BEHIND | `-` / `N` | Commits the base branch has that the PR head lacks (stale-but-mergeable when >0) |
| MERGE | `ready` / `behind N` / `blocked` / `pending` | Overall merge readiness (combines CI, conflicts, behind-ness, and review status) |

### `klaus dashboard`

Live TUI view of the PR pipeline. Groups runs by repository, auto-refreshes via filesystem watching and GitHub polling every 30s. Keyboard shortcuts: `j`/`k` (or `↑`/`↓`) move the PR selection, `a` approve the selected PR, `d` discuss the selected PR with the coordinator (pre-fills `WRT PR#<num>:` in the session pane and switches focus there), `o` open the selected PR in a browser, `r` force refresh, `q` quit.

When webhook mode is enabled, the data-source line shows a freshness indicator for the most recent webhook delivery (e.g. `· last event 5m`), color-coded as it ages (dim under 30m, yellow under 2h, red beyond). Note this surfaces the age of the last delivery, not delivery health — a long silence can mean a quiet repo or a broken delivery path, so it's a hint for a human to judge rather than a definitive check.

### `klaus approve`

Mark PRs as approved for merging. PR approval is a human task that exists only to ensure that all changes are gated by operator approval. By default, `klaus merge` requires approval before merging.

```bash
klaus approve 42 43                  # approve specific PRs
klaus approve --all                  # approve all merge-ready PRs
klaus approve --run 20260328-1603-a3f2  # approve by run ID
```

### `klaus merge`

Sequentially merges a list of PRs. Waits for CI to pass before merging (up to 10 min).

A PR that only *conflicts* is rebased onto main, verified, and force-pushed. Verification runs `merge_verify_command` when configured, else `go build ./...` when the repo has a `go.mod`, else nothing (relying on CI — so non-Go repos merge cleanly).

A PR that is merely *behind* main is merged as-is. Rebasing it would force-push a new head, which under a branch policy that dismisses reviews on new commits throws away the approval the merge depends on. If GitHub then refuses the merge because the branch must be up to date, klaus runs GitHub's "Update branch" (a merge from main, which keeps the head and the approval intact), waits for CI, and merges again.

By default, PRs must be approved with `klaus approve` before merging. Unapproved PRs trigger an interactive prompt (or are skipped with `--yes`).

`klaus merge` does not have to run inside a clone: pass `--repo owner/repo` (or let the repo be auto-detected from run state) and klaus resolves the checkout it needs — the current repo, a registered project, or a fresh clone under `worktree_base/.repos`. The target repo's `.klaus/config.json` is what applies, not the current directory's.

```bash
klaus merge 42 43 44
klaus merge --dry-run 42
klaus merge --merge-method rebase --no-delete-branch 42
klaus merge --force 42               # bypass approval check
klaus merge --yes 42 43              # skip unapproved PRs without prompting
```

Flags: `--dry-run`, `--merge-method` (squash/merge/rebase), `--no-delete-branch`, `--repo` (target repo for all PRs), `--force` (bypass approval), `--yes` (skip unapproved).

### `klaus project`

Manage a persistent registry of projects. The registry maps short names to local paths and is stored in `~/.klaus/projects.json`.

```bash
klaus project add owner/repo              # clone into projects dir and register
klaus project add owner/repo --path .     # register an existing local checkout
klaus project add owner/repo -d "CLI for X"  # register with an explicit description
klaus project add my-tool                 # search your GitHub repos by name
klaus project list                        # show all registered projects
klaus project remove my-tool              # unregister (does not delete the clone)
klaus project describe my-tool "CLI for X"  # add or update the one-line description
klaus project describe my-tool ""         # clear the description
klaus project set-dir ~/hack              # set the default clone directory
```

When `klaus project add` is called without `-d/--description`, klaus tries `gh repo view --json description` to pick up the GitHub description automatically. If gh is unavailable or the repo has no description, the project is registered without one — you can backfill later with `klaus project describe`. Descriptions show up in `klaus project list` and in the session coordinator's `## Registered projects` block.

### `klaus webhook`

Check and create GitHub webhooks for registered projects. Webhooks let the dashboard receive real-time events (CI status, PR reviews, pushes) instead of relying solely on polling.

```bash
klaus webhook check                  # show webhook status for all registered projects
klaus webhook setup                  # create missing webhooks for all projects
klaus webhook setup my-project       # create a webhook for a specific project
```

Both commands require `webhook.relay_url` in your config. `setup` also requires `webhook.secret_file` — a path to a file containing the HMAC secret used to verify incoming payloads.

```json
{
  "webhook": {
    "relay_url": "https://your-relay.example.com",
    "secret_file": "/path/to/webhook-secret"
  }
}
```

The created webhooks subscribe to `push`, `check_run`, `check_suite`, `pull_request`, `pull_request_review`, and `issue_comment` events. `pull_request_review` and `issue_comment` are what let comments from `trusted_reviewers` dispatch fix agents promptly: both inline review comments and PR conversation comments count (conversation comments are re-evaluated when created or edited). Webhooks created before `issue_comment` was added don't include it; add it to the hook's events in GitHub (or to your relay's forwarded events).

Additional webhook config knobs:

- `poll_fallback` (bool, default `false`): when `true`, the dashboard polls GitHub every 30s *in addition* to receiving webhooks.
- `reconcile_interval_seconds` (int, default `300`): in webhook-only mode (`poll_fallback: false`), a slow reconcile heartbeat runs a full PR status re-fetch on this interval. This bounds worst-case staleness if a webhook is ever dropped or missed — without reverting to full 30s polling. Set to a negative value to disable. Ignored when `poll_fallback` is `true` (polling already re-fetches every 30s).

```json
{
  "webhook": {
    "relay_url": "https://your-relay.example.com",
    "secret_file": "/path/to/webhook-secret",
    "poll_fallback": false,
    "reconcile_interval_seconds": 300
  }
}
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
  "auto_merge_on_approval": false,
  "merge_verify_command": "nix build",
  "default_agent_model": "claude-sonnet-5",
  "default_agent_effort": "medium",
  "agent_display": "detached"
}
```

`trusted_reviewers` lists GitHub logins whose comments dispatch a fix agent even without a formal "changes requested" review. Both inline review comments and PR conversation comments count. A comment stops counting once a newer commit is pushed; editing a comment counts as new, so feedback added to an older comment after the latest push is picked up.

Conversation comments by the PR author are the exception: the operator and fix agents post through the same GitHub account, so an author's conversation comment is ignored unless it opts in, even when the author is in `trusted_reviewers`. Opt in either by starting the comment with a `/klaus fix` line, or by putting the hidden marker `<!-- klaus-actionable -->` on a line of its own. Quoted lines (`> /klaus fix`) don't count. Trusted reviewers other than the PR author need no opt-in, and inline review comments are unaffected by this rule.

`merge_verify_command` runs in the rebased worktree during `klaus merge` to sanity-check the branch before force-push. When unset, klaus runs `go build ./...` only if a `go.mod` is present, otherwise skips the check (non-Go repos rely on CI).

`default_agent_model` / `default_agent_effort` set the `claude --model` / `--effort` for launched agents when the launch doesn't pass the corresponding flag. When unset, the flag is omitted from the `claude` command entirely.

`agent_display` controls where agent panes live. `detached` (the default) puts each agent in its own window of a detached tmux session named `klaus-agents-<session-id>`, so your coordinator window is never split. `pane` restores the old behaviour of splitting the coordinator's window for every agent.

**`.klaus/prompt.md`** — Custom system prompt for launched agents. Go template variables: `{{.RunID}}`, `{{.Issue}}`, `{{.Branch}}`, `{{.RepoName}}`. Customize this to match your repo's conventions, test commands, and PR workflow.

**`.klaus/session-prompt.md`** — Custom prompt for the coordinator session. Same template variables.

**`.klaus/pr-fix-prompt.md`** — Custom prompt for PR-fix agents. Additional variable: `{{.PR}}`.

## Under the hood

- **Agents run headless** — `claude -p`, `codex exec`, or `agy --print` with the prompt in argv, so there is no
  way to type at a running agent. Correcting one mid-run means relaunching with
  `klaus launch --resume-from <run-id>`; see [docs/AGENT_MESSAGING.md](docs/AGENT_MESSAGING.md)
  for why in-place messaging isn't offered and what it would take
- **Worktrees** isolate each agent — they can't step on each other or your working tree
- **tmux panes** manage each agent's process lifecycle. They live in a detached `klaus-agents-<session-id>` session owned by the tmux server, so they cost you no screen space and agents survive the coordinator exiting (`agent_display: "pane"` splits your window instead)
- **JSONL logs** are saved for replay and post-run analysis
- **Sensitivity scanning** checks logs for private IPs, SSH keys, and credentials before persisting
- **State storage** — session state lives in `~/.klaus/sessions/` (ephemeral, machine-local), while finalized run artifacts sync to the repo's data ref
- **Data ref** (`refs/klaus/data`) stores run metadata without polluting your branch list

## Requirements

- `tmux` (sessions run inside tmux)
- At least one configured backend CLI: `claude`, `codex`, or `agy` (authenticated)
- `git` (needed for agent worktrees; sessions can run without it)
- `gh` (GitHub CLI, for PR operations)

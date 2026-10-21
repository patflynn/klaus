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
| `--pr` / `--change` follow-up conversation | Replays the latest run's stored conversation, on any machine with the data ref | Forks the latest run's Codex session on the same machine | Fresh conversation |
| Shared coordinator memory | Claude memory directory | CLI's own memory behavior | CLI's own memory behavior |

For Codex and agy, explicit `--budget` and `--replay` are rejected; the legacy
`default_budget` does not apply, and unknown cost is displayed as unavailable.
agy workers use a 24-hour print timeout instead of its short CLI default.
`--resume-from` never transfers conversations between backends or machines;
when a compatible conversation is unavailable, the worker starts fresh. Use
`--pr` (or `--change` in a repo without PRs) to continue the actual branch
changes. agy coordinator IDs are recovered
from its workspace cache; missing cache metadata starts a fresh conversation
with a warning rather than attaching to an unrelated conversation. A resumed
agy coordinator receives a short continuation prompt instead of repeating the
original instructions.

Pre-PR peer review prefers a different family from the worker's (see below)
and falls back to the worker's own backend, so Codex workers do not require
Claude to be installed. Authenticate each selected CLI beforehand and
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
family. All three backends can review. The model is
`backends.<reviewer>.review_model`, else
`pre_review.review_model` for Claude (default `haiku`), else the CLI default.
The same choice drives the `_pre-review` agents run before opening a PR; it
prints the reviewer it used.

`klaus review <pr>` gets a second opinion on an open PR:

```bash
klaus review 303                                  # reviewer chosen as above
klaus review 303 --repo owner/repo --backend codex  # explicit reviewer
klaus review 303 --post                           # submit it to the PR
```

The PR's author family comes from the klaus run that opened it (Claude if none).
klaus refuses a same-family review unless you pass `--backend` together with
`--allow-same-family`. The reviewer sees only `gh pr diff` plus the PR title and
description and runs read-only in an empty temp directory, using the same
isolation as [consultations](#consulting-other-models). It returns findings and a
short verdict on whether the change matches its stated intent.

`--post` (default: `cross_review.post`) submits one GitHub review with event
`COMMENT`: findings on lines in the diff become inline comments, the rest go in
the body next to the verdict. **Cross-model reviews are second opinions for the
operator, never approvals**; klaus never posts `APPROVE` or `REQUEST_CHANGES`,
and approval stays with `klaus approve`. Posted inline findings feed the normal
trusted-review fix loop (see [docs/PIPELINE.md](docs/PIPELINE.md#4-review--approval)).
Posting is refused when the same reviewer already reviewed the PR's head
commit, or when the PR already has `max_rounds` cross-reviews posted from your
gh account. That check runs again right before posting, along with a check that
the PR head hasn't moved, so stale findings are never posted after a push. A per-PR lock under the
session directory makes a second concurrent `--post` in the same session fail
immediately.

Inside a Klaus session, `klaus review` emits `review:started` and
`review:completed` (with `backend`, `model`, `repo`, `pr_number`, `author`,
`post`, and at the end `duration_ms`, `success`, `findings`, `review_url` or
`error`), and leaves a job record like a consult's with its verdict, findings
and summary (see [Consulting other models](#consulting-other-models)). SIGINT
and SIGTERM end it as `interrupted`.

The coordinator prompt describes `klaus watch`. Claude can attach its Monitor
tool; other backends use their own background tools or `klaus status` and
`klaus logs`. Klaus does not emulate Claude's Monitor tool.

CLI event contracts: [Codex non-interactive mode](https://developers.openai.com/codex/noninteractive)
and [Antigravity headless mode](https://antigravity.google/docs/cli/headless).

### Consulting other models

Use `klaus consult` for a read-only thinking partner without a worker, worktree,
or branch. It chooses an installed family other than `KLAUS_BACKEND`, in the
configured order; `--backend` overrides that choice. Model and effort use the
same per-backend defaults as launch.

```bash
klaus consult --role critic --file plan.md --thread auth "Critique this plan"
klaus consult --panel "Should we use polling or webhooks for build status?"
```

Configure selection and the default role in `~/.klaus/config.json` or
`.klaus/config.json`:

```json
{
  "consult": { "order": ["codex","claude","agy"], "default_role": "partner" }
}
```

Consultations and reviews share the same read-only isolation.
Codex uses its read-only sandbox and `--ignore-user-config` to avoid loading
MCP servers from `$CODEX_HOME/config.toml`; authentication still uses
`CODEX_HOME`. Model and effort come from Klaus flags/defaults, and apps, plugins,
hooks, and subagents are explicitly disabled. Claude uses safe mode (no custom
hooks/plugins) and exposes only Read/Grep/Glob; agy
uses plan mode and a temporary primary-agent definition exposing only file reads
and search. The agy definition is removed after the call. Every backend reads
the question on stdin, so no prompt meets the OS argument-size limit.

Roles are `partner`, `critic`, `reviewer`, or your own system-prompt text.
`--dir` selects a workspace (default: cwd); `--repo` resolves a registered local
project. Repeat `--file` to inline attachments, up to 200KB total, or supply the
question via `--prompt-file`. File paths resolve from the invoking directory.
Each attachment goes in the prompt under a header with its path, so the partner
needn't open it. Headless agy can't read a path outside its workspace, and a
denied read ends its turn with no answer, so for agy each file sits between
BEGIN and END lines, behind a note that it is there in full.

Keep a `--thread NAME` per topic. Threads retain their backend, model, effort,
role, and workspace; incompatible overrides are rejected. Metadata and readable
transcripts live in `~/.klaus/sessions/<session-id>/consults/NAME.{json,log}`.
New transcript turns are JSON lines, persisted as `pending` before the backend
starts and atomically updated to `ok` or `error: <message>`. Each records backend,
model, resume ID, repository revision, and session-ID source (`stderr`,
`stdout-json`, `diagnostic-log`, or `text`; `assigned`/`resume` identify known IDs).
An interrupted turn stays `pending`; legacy transcript text is retained. Failed
turns preserve stdout/stderr in `NAME.<turn>.raw`, and extraction errors report
the paths tried and raw-output filename. Turn numbers include failed attempts;
`--list` counts successful turns.

`klaus consult --list` shows backend, turns, and last-used time. Outside a pane,
threads use the most recent Klaus session; one-shot questions need no session.

`--panel` queries every installed family other than the caller concurrently and
prints responses as they finish under backend/model headings. Responses are
buffered to avoid interleaving; a failed member makes the command fail after
printing the other answers. `--timeout` sets a positive per-backend deadline
(default `10m`, e.g. `--timeout 30s`); timed-out panel members print `timed out`
while the others finish. The deadline also applies to single-backend consults.
Panel mode cannot be combined with `--thread` or
`--backend`. Consults never create agent run state.

Inside a Klaus session, each backend call emits a `consult:started` event
before the model runs and a `consult:completed` event when it ends; both appear
in the default `klaus watch` filter. The pair shares a `run_id` of the form
`consult-YYYYMMDD-HHMM-xxxxxxxx`, and a panel emits one pair per member. Both
events carry `backend`, `model`, `effort` and `role` (after defaults), `thread`
(empty for one-shot), `dir` (absolute workspace), `repo` (with `--repo`),
`panel`, `prompt` (first line, up to 120 characters) and `prompt_file` (with
`--prompt-file`). `consult:completed` adds `duration_ms`, `success`, and on
failure a short `error` such as `timeout after 10m0s` or `interrupted`; it never
contains model output. SIGINT and SIGTERM still produce the completion, so
every start has a matching completion unless the process is killed with
SIGKILL. Consults outside a session emit nothing.

Each backend call also leaves a job record in
`~/.klaus/sessions/<session-id>/jobs/<run_id>.json`, written when it starts and
again when it ends, so you can see what is running and how it ended: `klaus
status` and the dashboard list every running job and those that finished in the
last 3 hours, and `klaus jobs` lists them on their own (`--all` for every record
kept). A record holds `kind` (`consult` or `review`), `thread` and `turn`,
`role`, `backend`, `model`, `effort`, `dir` or `repo` (a review's `pr`), the
`prompt` excerpt, `started_at` and `finished_at`, the klaus `pid` (with its
start time from `/proc/<pid>/stat` on Linux, so a reused PID isn't mistaken
for it), and `status`: `running`, `succeeded`, `failed`, `timed out` or
`interrupted`, with a short `error`. A record still `running` whose process is
gone, as after SIGKILL, reads as `died`. Panel members share a `panel` ID.

`klaus jobs show <id>` prints a record and its answer, and `--answer` only the
answer, so a background consult needs no redirect of its own: a thread's answer
is read from its log, a one-shot consult's from `<run_id>.answer` beside the
record, and a failed one-shot consult keeps what its backend wrote in
`<run_id>.raw`. `<id>` can be a panel ID (every member is shown) or a unique
ending of a job ID, such as the four characters the dashboard shows. Records of
jobs that finished more than 7 days ago are removed when a new job starts.

```bash
klaus consult --thread design "Draft the migration plan" &   # runs in the background
klaus jobs                                                    # what's running, what ended
klaus jobs show 1a2b --answer                                 # read it when it's done
```

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

Each fix agent's run records why it was dispatched (`ci-fix`, `review-fix`, `review-comment-fix` or `conflict-fix`); see [Where agent time goes](#where-agent-time-goes).

When a `pr-fix` run is already active on a PR — including coordinator-launched runs from `klaus launch --pr` — the pipeline will not auto-dispatch additional fix or rebase agents on it. This prevents races during multi-step refactors where intermediate commits may fail CI before the run completes.

Pipeline stages per PR: `ci_pending` → `ci_passed` → `approved` → `merged`, with failure paths back through `ci_failed` or `changes_requested`. Budget-exhausted agents land in `budget_paused` — see [Budget pause and resume](#budget-pause-and-resume) below. A PR whose merge or branch update keeps failing is retried once a minute for three attempts, then moves to `stalled` and emits a single `pipeline:stalled` event for a human to pick up.

You can also drive the pipeline manually with `klaus approve` and `klaus merge`.

### Real-time event channel

Pipeline events (PR created/approved/merged, CI passed/failed, branches pushed without a PR, agents needing attention, agent errors), consult events (`consult:started`, `consult:completed`) and review events (`review:started`, `review:completed`) are appended to `~/.klaus/sessions/$KLAUS_SESSION_ID/events.jsonl` and exposed as a streaming channel via `klaus watch`. It's designed for [Claude Code's Monitor tool](https://docs.anthropic.com/en/docs/claude-code) — each matching event becomes a notification injected into the coordinator's context between turns, so the coordinator can react to merges or CI failures without you having to mention them.

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

### Where agent time goes

Every run event (`agent:*`, `phase:*`, `check:*`, and `pr:approval-changed` from `klaus approve`) carries fields that say what the run worked on and why, alongside its existing fields:

| Field | Meaning |
|---|---|
| `repo_key` | The repo, qualified: `owner/repo` on GitHub, `valley:<name>` for a repo that delivers to a valley, else the repo's short name |
| `change_id`, `pr_number` | The run's change, and its PR once it has one |
| `key` | The change, qualified by `repo_key`: `owner/repo#12` once the run has a PR, else `<repo_key>@<change_id>` |
| `dispatch_reason` | Why the run was launched (below) |
| `head_sha` | The branch tip finalize found on `origin`, else the worktree's `HEAD` when the agent finished, else when it started |

The pipeline's PR events (`pr:approved`, `pr:merged`, `agent:ci-*`, ...) carry `repo_key` and `key` when their PR URL is known; `review:*` events carry them for the reviewed PR, and `consult:*` events carry `repo_key` for the consult's workspace. Fields that aren't known are left out, and no existing field changed.

The dispatch reason is `launch` for new work, `pr-follow-up` for `--pr`, `budget-resume` for `--pr` on a budget-paused PR, `change-follow-up` for `--change` and `resume` for `--resume-from`; the pipeline's fix agents are `ci-fix`, `review-fix` (changes requested), `review-comment-fix` (a trusted reviewer's comments) and `conflict-fix`. `klaus launch --reason <reason>` names any other, such as `docs-timer` for a timer's launch.

Each run's phases are events too: `phase:completed` with `phase: setup` (launch's work before the agent starts: fetch, worktree, hooks, pane), then `phase:started`/`phase:completed` for `agent` (pane start to finalize; with `cost_usd` where the backend reports it) and `finalize` (up to killing the pane). Each `phase:completed` carries `started_at`, `ended_at` and `duration_ms`. A launch that fails records no run, so setup has only a completion.

Checks an agent runs, where klaus can see them, are `check:started`/`check:completed` with `kind`, `command`, `source`, and at the end `duration_ms` and `success` when known: `klaus _pre-review` reports itself (`kind: pre-review`, with `lint_ms`, `review_ms`, `findings`, `blocking` and `lint_failed`), and `_format-stream` times the build, test and lint commands it sees in the agent's stream (`go test`, `go vet`, `cargo build`, `npm test`, `make <target>`, `nix flake check`, ...) from the line that calls one to the line with its result. Launch exports `KLAUS_RUN_ID` in the agent's pane so both know the run. Launch saves the run just after the pane starts, so a check that ends first waits up to two seconds for it, then is recorded under the run's ID without the run's fields, which `klaus report time` takes from the run's other events. These events are not in `klaus watch`'s default filter.

`klaus report time` reads the event logs of every session (`--current` for the current one only) and sums, per repo and overall, the runs that started in the window (`--since`, default `7d`; `0` for all time) and the consults and reviews: runs by dispatch reason, time by phase, checks by kind, the time a change waited between one run ending and the next starting, the time from a PR's last run to its first approval, follow-up runs per change and their reasons, consult and review time, and cost where known. Runs are grouped into changes by change ID, else PR. `--repo` keeps one repo, named by key, valley name or short name; `--json` prints the report with durations in milliseconds.

```bash
klaus report time                      # the last week, every session
klaus report time --since 2w --repo klaus
klaus report time --current --json
```

Events from before these fields exist still count: a run without phase events has its agent time from `agent:started` to its last event, a run without a recorded reason counts as `unknown`, and a run that names its repo only by short name takes the qualified key other runs give that name when there is just one.

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

By default, `klaus launch --pr` against a budget-paused PR, and [`klaus launch --change`](#klaus-launch---change) on any change, does more than pick up the branch's work: it **continues the previous agent's Claude conversation** instead of starting one cold. At finalize time klaus commits the resume-able conversation trajectory (the JSONL `claude` itself writes under `~/.claude/projects/…`) to the local clone's `refs/klaus/data` at `sessions/<run-id>.jsonl`. On resume it makes a best-effort fetch of the ref from origin, reads that blob from the local ref, restores it into the new worktree's project dir, and invokes `claude --resume <uuid>`. The conversation continues from the exact point of pause — no re-grepping or re-orienting — which is faster and cheaper than a fresh agent.

Only the most recent run on the PR (or the change) is replayed: an older conversation never saw the work that came after it, so when the most recent run's conversation can't be resumed, the agent starts fresh rather than resuming an older one. Replay is best-effort and **falls back to a fresh agent** when any of these hold; the `replay:` line names the run and the reason:

- The most recent run used another backend: a Claude worker can't resume a Codex conversation, or the reverse. A Codex worker continuing a Codex run forks that run's session (`codex exec fork <thread>`) instead of replaying a stored trajectory; that works on the machine that ran it, not on a sandbox host. agy workers can't resume a conversation and always start fresh.
- The trajectory was never stored (skipped because `klaus` detected potentially sensitive content), or the PR pre-dates this feature.
- The paused agent ran on another machine. The data ref stays in the clone that ran the agent unless [`push_data_ref`](#configuration) is true there, so replaying on a different machine needs that push.
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

### Runs that end without a PR

`klaus launch` reads the target repo's `origin` to decide how the agent delivers. A github.com origin (`git@github.com:o/r.git`, `https://github.com/o/r`, `ssh://git@github.com/o/r.git`) gets the PR workflow. Any other origin, such as a plain ssh remote like `git@10.100.0.1:cosmo.git` or a local path, is a direct-push repo: the default worker prompt tells the agent to push its branch, not to open a PR or use `gh`, and to end with a report of the branch, head SHA, diff summary and every check it ran, which is what you review before integrating. Launch prints `delivery: push branch, no PR` for these runs. Sandbox runs are decided from the same local repo their worktree is made from. Launch records the decision on the run as `delivery` (`pr` or `push`; a `--pr` fix is always `pr`), and `_finalize` honours it: a run that delivers by push never records a PR, even when its log carries a GitHub PR URL, such as a link the agent read or a test fixture. A run recorded before `delivery` existed is decided from its clone's `origin` the same way.

A run that finishes cleanly with its branch already on `origin` — the normal finish in a direct-push repo, where you integrate branches by hand instead of through PRs — is not salvaged. `_finalize` checks `origin` with `git ls-remote` before cleanup, records the branch and its head SHA on the run (`pushed_branch`, `pushed_sha`), and emits `agent:completed` plus `agent:branch-pushed` with `repo`, `branch` and `sha`. `klaus status` shows the run as `awaiting-integration`, and the dashboard lists it as yours to submit: `SUBMIT <branch>@<short sha>` (a repo that delivers to a valley goes further; see [Repos that deliver to a valley](#repos-that-deliver-to-a-valley)). The local branch is deleted; the branch on `origin` is the record. Runs with a PR emit `agent:pr-created` instead, never both.

Whether a pushed branch has landed is observed, not recorded: `klaus status` and the dashboard check it against the run's own clone (`clone_dir`) each time they look, and nothing is written to the run. One check per repo takes a snapshot of `origin` with `git ls-remote`, fetches the default branch and the pushed branches that still exist, and then judges each run:

- **integrated** (`INTEGRATED` on the dashboard): the pushed SHA, or the branch's current tip, is an ancestor of the default branch. The current tip counts because you may rebase the branch onto the default branch and force-push it before it fast-forwards.
- **integrated by content** (`INTEGRATED~`): a first-parent commit on the default branch, made after the run started, has the same `git patch-id --stable` as the branch's change (its diff from the merge base). This catches an integrator that lands a change as one new commit on the default branch. It is strong evidence, not proof. If the default branch changed lines next to the edit, the patch-ids differ and the run keeps waiting; check it yourself.
- **branch-gone** (`GONE`): the branch was deleted from `origin` and neither check matched, for example a rejected or superseded change.
- otherwise **awaiting-integration** (`SUBMIT`).

If the clone is missing or `origin` can't be listed or fetched, the run stays awaiting; an incomplete check never reports a branch as gone. A deleted branch also stays awaiting when the clone can't rule out that it landed: the pushed SHA was pruned from the clone, the clone's history is too shallow to find a merge base with the default branch, or the content check stopped at its cap of 500 default-branch commits before reaching the run's start.

#### Repos that deliver to a valley

A direct-push repo can deliver to a valley, the self-hosted integration host from the-valley, instead of being integrated by hand. Finalize treats a repo as a valley's when its `origin` isn't GitHub, the pushed head carries `policy/valley` or a `policy/instance/` directory, and the `valley` on `PATH` lists the `ready` and `changes` verbs in `valley help`. Any other repo, including one whose `valley` predates those verbs, is handled as described above.

In a valley's repo, once `agent:branch-pushed` is out, finalize marks the pushed head ready as a version of the run's change:

```
valley ready --change <change_id> --head <pushed_sha> <branch>
```

The change is the run's `change_id`, the jj-form ID klaus stamps on every agent commit as its `Change-Id` trailer. The valley identifies the pusher from git config `valley.principal`, which the host sets, not klaus. On success valley prints `<repo> <handle> v<n> ready`. Finalize records the repo, the change's handle, the version and the head's subject (its title) on the run as `valley`, and emits `agent:change-ready` with `repo`, `change_id`, `handle`, `version`, `title` and `head`. The call is bounded by 30 seconds. If valley refuses, times out or prints anything else, finalize records a short reason as `valley.error`, prints a warning, and finishes as usual; the dashboard shows the run as `SUBMIT` with the reason.

`klaus status` and the dashboard then ask the valley where each change stands, with one `valley changes --all --json` per check, covering every repo. They read the `valley-changes/v1` view and refuse any other schema. A run's version is the one in its repo, with its change ID, whose head is the run's pushed SHA. The run's repo is the one `valley ready` reported; or else, for a run that continues another's change, that run's repo, which launch copies onto the new run as `change_repo`; or else the one launch's `valley unready` reported; or else the last component of its target repo without `.git`; a run whose repo klaus doesn't know is never matched, and neither is a run in a repo the listing marks with an `error`, whatever records it lists beside it:

| Dashboard | `klaus status` | Group | The valley's record |
|-----------|----------------|-------|---------------------|
| `READY` | `awaiting-approval` | yours | the run's version is its change's active one, ready and waiting for your approval |
| `APPROVED` | `approved` | pipeline | an integration request is filed; the integrator hasn't decided it |
| `INTEGRATED` | `integrated` | done | the version landed (`landed` or `landed-recorded`) |
| `UNCERTAIN` | `uncertain` | yours | a request was consumed or failed with no landing evidence, or the record holds a value klaus doesn't know, on a change that isn't a draft; look at it |
| `STALE`, `REJECTED`, `FAILED` | `stale`, `rejected`, `failed` | yours | the integrator's decision on the request |
| `WITHDRAWN` | `withdrawn` | done | the change was withdrawn |
| — | `superseded` | done | another version of the change is the active one; the dashboard shows the change's row as that version stands |
| `DRAFT` | `draft` | yours | the change was returned to draft (`valley unready`), as launch does for a follow-up run, and no run of it is running; whatever the valley says of its active version short of landing, even `uncertain`, there is nothing for you to do: approve refuses it until a head is ready again |

A change in a state klaus doesn't know isn't matched. A version that landed shows `INTEGRATED` even after a later version replaced it, or its change went back to draft. The valley is asked only when finalize found one for some run. If valley fails, or a run isn't in its listing, the run is checked against its clone as described above, and a change waiting on that check reads `READY` (`awaiting-approval`). A deleted branch alone never settles a change finalize marked ready: it stays yours (`awaiting-approval`) rather than `GONE`. When the dashboard has seen the valley's record of a change and a later check can't place it, the row keeps that last verdict, marked `(last known)`, unless git finds the work landed. The dashboard logs a failed listing at debug level.

##### Follow-up runs on a change

A run that continues a change — `--change`, `--pr`, `--resume-from`, or a pipeline fix, any launch whose change ID is inherited (see [Provenance trailers](#provenance-trailers)) — pushes more commits to it. On the dashboard, the runs without a PR on one change share a row, as the runs on a PR share the PR's row (see [`klaus dashboard`](#klaus-dashboard)): while a follow-up runs, the change's row names it (`agent:<short run id> running`), sorts with the agents and never offers the approve command, and the footer says which agent is working on it (`agent:<short run id> is working on this change`). When it exits, the row goes back to where the change stands. Runs share a change when they have the same change ID in the same repo, the repo named as above (the one `valley ready` reported, else the repo of the run a follow-up continues, else the one `valley unready` reported, else the last component of the target repo); a run without a change ID shares it with none. A follow-up launched from a run takes that run's repo even when its own target repo has another name (a valley's `widget` launched as `acme/source`) and `valley unready` didn't run or failed. A run is running when its pane is running it, judged the same way by the dashboard and `klaus status`: a pane whose command exited, or a finished run's pane left idle at a shell, isn't running. `klaus status` stays per run: a run that isn't running or done shows `working` while another run of its change is running.

In a valley's repo, launch also returns an inherited change to draft before the agent starts, so the valley's `approve` refuses it while the agent replaces it:

```
valley unready <change_id>
```

Launch checks the repo as finalize does, against the commit the worktree starts from, and runs this only when `valley help` also lists `unready`. On success valley prints `<repo> <handle> v<N> draft`, naming the version that was ready (a valley that names none prints `<repo> <handle> draft`), and launch records the repo and handle on the run as `unready`. A change that wasn't ready (already a draft, or no record) is noted and left alone. The call is bounded by 30 seconds; a refusal, timeout, unexpected output or other failure is recorded as `unready.error` and printed as a warning, and the launch goes on. Finalize's `valley ready` marks the pushed head ready again: the valley re-readies the same version when the head is unchanged, or adds a version.

When a run finishes without a PR for any other reason (for example a Claude session-limit stop, or uncommitted changes left behind) or crashes, `_finalize` salvages the worktree instead of discarding it:

1. Commits any uncommitted changes (`wip: klaus finalize salvage <id>`).
2. Pushes the branch to `origin` (plain `git push -u`, never forced) unless `origin`'s live tip already matches. Push failures are logged; nothing is deleted. Branches of runs that already have a PR are not pushed.
3. Keeps the local branch and removes the worktree (the worktree is kept if the WIP commit failed). Finalize never deletes a branch unless its work is on the default branch or `origin`'s live tip (checked with `ls-remote`, not cached `origin/*` refs) matches it; an unreachable remote counts as not pushed.
4. Emits `agent:needs-attention` with `branch`, `pushed`, and `reason` (`no_pr`, `session_limit`, `background work still running (<commands>)`, or the crash reason) instead of `agent:completed`. `klaus status` shows `needs-attention`; the dashboard shows `ATTN`.

A run ends when the agent's turn ends, so an agent that stops while a build it started is still running never sees the result, and cleanup interrupts the build. On Linux, finalize looks for processes still running inside the worktree and records them as the run's `failure_reason`, for example `background work still running (nix)`.

Runs that ended without a PR, left no commits beyond the default branch, and left no background work running still finish as `agent:completed`.

There is no subcommand named `klaus resume` or `klaus finalize` — resuming happens through flags on `klaus launch`, and `_finalize` handles the WIP commit automatically:

- `klaus launch --pr <num> "..."` resumes a budget-paused PR (WIP commit + trajectory replay, as above).
- `klaus launch --change <ref> "..."` continues a change in a repo without PRs on its own branch, replaying the conversation of its most recent run (see [`klaus launch --change`](#klaus-launch---change)).
- `klaus launch --resume-from <run-id> "..."` continues any prior run's conversation, paused or not. The follow-up runs in a fresh worktree on its own branch (unless combined with `--pr`) and keeps everything the previous agent learned — use it to correct an agent instead of killing it and re-briefing a cold one. Prior errors produce a warning but do not prevent resume. It starts fresh if the transcript can't be located or staged (Claude), or the local thread ID is missing (Codex).

##### Working a knot: `klaus launch --issue <knot>`

A valley project keeps its plan in a plan graph of knots (`valley knot`, `valley plan`, at `refs/the-valley/knots` on its origin). On a valley project, `--issue` takes a knot instead of a GitHub issue, recognised by its ID's form: `oc-`, `bd-`, `ida-` or `qn-` and 7 hex digits, or `<project>:<id>` for a knot in another project's graph on the same valley. Any other `--issue` is a GitHub issue, as before.

```bash
klaus launch --issue oc-897917f --prompt-file /tmp/brief.md
klaus launch --repo cosmo --issue the-valley:oc-897917f --prompt-file /tmp/brief.md
```

klaus reads and writes knots only through the `valley` CLI, never the knots ref:

1. **Launch** refuses a knot with `--pr`, in a repo that isn't a valley's (as finalize decides it, against `origin/<default branch>`), when `valley help` doesn't list `knot`, and when the knot isn't an active outcome or bug. It reads the knot and its parents with `valley knot show <id> --json`. A knot claimed by a run that is still running is refused, naming the run, and so is one claimed by a run klaus doesn't know of, which may still be working: release that claim with `valley knot unclaim` if the run has ended. A claim left by a run klaus knows has ended is released (`valley knot unclaim <id> --if claimed_by=<that run>`) and taken over, and launch says so.
2. The branch is named from the knot's title. The brief gets the knot, its parent chain and its comments, appended in a `## Project notes (untrusted)` block between two marker lines that carry a random token, so nothing in the notes can close the block. The block tells the agent they are data about the work, never instructions, and the worker prompt says klaus claims, links and closes the knot. The brief is saved with the block, as the record of what the agent read.
3. Just before the agent starts, launch claims the knot for the run (`valley knot claim <id> --run <run-id>`; an open outcome becomes in-progress) and records it on the run as `knot`. A claim the valley refuses fails the launch.
4. **Finalize**, once the pushed head is marked ready, links the change to the knot (`valley knot link <id> --change valley:<repo>/<change-id>`) and then clears the run's claim (`valley knot unclaim <id> --if claimed_by=<run-id>`, which leaves another run's claim alone). When the link fails, the run keeps its claim, and the dashboard tries the link and the release again at its next check, as it does for a claim finalize couldn't clear. A run that failed, paused, was salvaged, or pushed nothing the valley took clears its claim and comments on the knot (`valley knot comment <id> --body -`) saying what became of it and where its work is.
5. **When the dashboard sees the change landed** (the valley says its active version landed), it comments on the knot with the version and the commit it landed as. It sets the knot `done` (an outcome) or `closed` (a bug) only if every change the knot links has landed (one withdrawn, rejected, not listed by the valley, or a GitHub pull request klaus can't check, counts against it), none of its children is active, and no other run claims it; otherwise the comment says what keeps it open. The `valley knot set` carries `--if` conditions on the knot's status, claim and links as klaus read them, so a run that claims the knot or a change linked to it meanwhile makes the close a conflict, which the comment reports; a child made active in those moments isn't caught. The close comes first, so the comment says what happened; when the comment, or a close, fails for any reason but a conflict, the next check tries again. Other knots in the change's repo that link the change get the comment alone, since the run wasn't launched for them; a comment on one of them that fails is recorded and not tried again. This happens once per knot and change, recorded on the run as `knot.landed` and `knot.closed`.

Every knot call is bounded by 30 seconds. At finalize and after landing, a failure is recorded on the run as `knot.error` and never stops the run's finalize or the dashboard. A failed run's comment, or the release of its claim, isn't tried again: a claim it leaves behind is taken over by the next launch on the knot, as the claim of a run that has ended.

## Provenance trailers

Every commit an agent makes ends with two trailers:

```
Change-Id: <change id>
Klaus-Run: <run id>
```

From any commit, `Klaus-Run` leads to the run that made it, and through the run to its prompt, log, cost and trajectory. `Change-Id` names the change the commit belongs to, in jj's change-id form: 32 characters from `k` to `z`. A tool that tracks changes can compare it with a jj `change-id` header as it is.

The trailers are labels for finding and auditing commits. They grant nothing: no tool may trust a commit or skip a check because of one.

klaus adds them mechanically, never through the agent's prompt. `klaus launch` installs a `commit-msg` hook in the agent's worktree. On each commit the hook strips Claude/Anthropic attribution, adds `Change-Id` unless the message already has one, adds `Klaus-Run` unless that run is already listed, and then runs the repository's own `commit-msg` hook, if it has one. A commit amended by a later run therefore keeps its `Change-Id` and lists both runs in order. A message that is empty or made only of lines starting with `#` gets no trailers: git strips `#` lines from a message written in the editor but keeps them for `git commit -m`, and a hook cannot tell which applies, so the hook leaves such a message alone rather than turn an aborted commit into one holding only trailers. The commits `_finalize` makes when it salvages unfinished work or pauses at the budget cap skip hooks, so `_finalize` adds the same trailers itself.

Each run records its change ID as `change_id` in its state. A fresh launch gets a new one. A follow-up keeps the change ID of the work it continues:

- `--pr <num>` uses the change ID of the run that opened the PR, found in any session on the machine. If no run on the PR has one, it uses the `Change-Id` on the PR's head commit.
- `--change <ref>` uses the change ID of the change it names, even with `--resume-from`.
- `--resume-from <run-id>` uses the change ID of the run it resumes.
- A budget-paused replay is a `--pr` run, so it keeps the paused PR's change ID.

A follow-up to a run whose state predates change IDs gets a new one, and `klaus launch` says so in its `change:` line.

The hook applies to the agent's worktree only. klaus sets `core.hooksPath` with `git config --worktree`, which needs `extensions.worktreeConfig`; klaus enables that once on the clone. Other worktrees and the main clone keep their own hooks. Earlier klaus versions set `core.hooksPath` in the clone's shared config, pointing into one worktree's git dir. When the shared value points inside `.git/worktrees/`, klaus removes it the next time it installs the hook. Any other shared value is the repository's own, so klaus keeps it and runs the hook it names. The coordinator's session worktree gets the same hook without trailers, because the coordinator is not a run that produces a change.

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

### Self-update from upstream

A host can follow a branch of klaus without a system rebuild. Turn it on in the host config, `~/.klaus/config.json` (a repo's `.klaus/config.json` can't set it):

```json
"self_update": { "repo": "https://github.com/patflynn/klaus", "branch": "main" }
```

`repo` is an `https://`, `ssh://`, `git://` or `file://` URL, an absolute path, or scp-like `user@host:path`, which is read as `ssh://user@host/path` (write `user@host:~/path` for a path in your home on a plain server). Any other transport (`ext::`, `fd::`) and any character outside `[A-Za-z0-9._~/:@-]` (which nix's flake-ref parser would re-encode, as `+` to `%2B`, or read as syntax) is refused, and `git ls-remote` and nix read the repo by the same URL, so the source nix builds is exactly the one `ls-remote` read, and every git self-update runs, directly or through nix, has `GIT_ALLOW_PROTOCOL=https:ssh:git:file`. `branch` defaults to `main`. Then `klaus self-update`, typically from a timer every few minutes:

1. takes a lock under `$XDG_STATE_HOME/klaus/`, so runs never overlap; a run that finds another in progress exits 0 and says so;
2. resolves the branch's commit with `git ls-remote <repo> refs/heads/<branch>`;
3. exits 0 when the installed klaus was built from that commit (quietly; `-v` says so), or when that commit already failed to build or test, naming the failure; a failed commit is retried when the branch moves, or with `--retry`;
4. builds exactly that commit: `nix build --no-link --print-out-paths 'git+<repo>?ref=refs/heads/<branch>&rev=<sha>#packages.<system>.default'`;
5. smoke-tests the build: its `klaus --version` must report the commit's short SHA, and its `klaus status` must exit 0, which shows it reads the existing run state. A failure records the commit as failed, installs nothing, and exits non-zero;
6. installs it into the user's default nix profile in place of the profile element named `klaus`, leaving other elements alone. The system klaus stays underneath on PATH as the fallback;
7. records the current and previous store paths and commits, the time and any failure in `$XDG_STATE_HOME/klaus/self-update.json`.

Each step prints one short line, so the timer's journal reads as a history:

```
main is at 1a2b3c4 (installed: e949dba)
building git+https://github.com/patflynn/klaus?ref=refs/heads/main&rev=1a2b3c4…#packages.x86_64-linux.default
built /nix/store/…-klaus-0.4.1-1a2b3c4
smoke test passed: klaus version 0.4.1-1a2b3c4; klaus status exits 0
installed 1a2b3c4 into the nix profile, replacing e949dba
```

The trust boundary: only the configured branch installs, pinned to the commit `ls-remote` returned, and only after it builds and passes the smoke test. Nothing else, such as a PR head, installs by itself.

`klaus self-update --rollback` reinstalls the previous store path; a second `--rollback` returns to the newer one. `klaus self-update --pin` leaves the current install alone until `--unpin`: runs while pinned exit 0 with a note, so a bad tip isn't reinstalled while it gets fixed. Without the pin, the next run after a rollback installs the branch's tip again.

A running `klaus dashboard` restarts onto the new klaus by itself (see [`klaus dashboard`](#klaus-dashboard)). Agents already running finish on whichever klaus their pane resolved; new launches resolve `klaus` from PATH, so they use the new one at once.

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
| `klaus launch --change <ref> "<prompt>"` | Continue a change in a repo without PRs, on its branch |
| `klaus launch --branch <name> "<prompt>"` | Name the new run's branch instead of deriving it from the brief |
| `klaus launch --model <name> --effort <level> "<prompt>"` | Pick the model and reasoning effort for the agent |
| `klaus launch --reason <reason> "<prompt>"` | Say why the run is dispatched (e.g. `docs-timer`), for its events and `klaus report time` |
| `klaus target owner/repo` | Set session-level default target repo |
| `klaus status` | Dashboard of all runs (with CI, conflict, and merge-readiness columns), then running and recent consults and reviews |
| `klaus jobs [--all]` | List running and recent consults and reviews |
| `klaus jobs show <id> [--answer]` | Print a consult's or review's record and its answer |
| `klaus report time [--since 7d] [--repo <r>] [--current] [--json]` | Where agent wall-clock time went, per repo and overall (see [Where agent time goes](#where-agent-time-goes)) |
| `klaus logs <id>` | View agent output (live, replay, or raw) |
| `klaus cleanup <id>\|--all` | Tear down worktrees, panes, and state |
| `klaus push-log <id> [--push]` | Store a log held back for sensitivity on the data ref (`--push` also pushes the ref to origin) |
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
| `klaus self-update [--rollback\|--pin\|--unpin\|--retry]` | Install the tip of the configured klaus branch into the nix profile (see [Self-update from upstream](#self-update-from-upstream)) |

### `klaus launch --pr`

Push fixes to an existing PR's branch instead of creating a new PR. The agent checks out the PR's branch, makes changes, and pushes directly — the PR updates automatically. Useful for addressing review comments or fixing CI failures on an existing PR.

```bash
klaus launch --pr 42 "Address the review comments"
klaus launch --pr 42 --issue 10 "Fix the auth bug mentioned in review"
```

The `--pr` and `--issue` flags can coexist (the agent may reference the issue in commits).

#### A run still working on the branch

`--pr` and `--change` refuse to launch while a run in any session is still running (judged as the dashboard judges it) on the branch they would work on, or on the change they continue, in the target repo; the error names that run. A `--pr` launch's change is that of the run that opened the PR. Wait for the run to finish, or stop it, or pass `--force-takeover` to launch anyway: the new worktree replaces the running agent's, **discarding that run's local, uncommitted work** and any commits it hasn't pushed.

Without `--force-takeover`, creating the worktree never discards work origin lacks, whoever left it: when the branch is checked out in a worktree with uncommitted changes, or the local branch has commits that aren't on `origin/<branch>`, launch refuses and names the worktree or branch. A worktree on the branch that holds nothing origin lacks is removed to make way, and a local branch that only points at what origin has is reset to origin's tip.

### `klaus launch --change`

Continue an existing change in a repo that delivers without PRs (its origin isn't on GitHub, as in a [valley's repo](#repos-that-deliver-to-a-valley)), the way `--pr` continues a PR. Use it for every follow-up on a change, instead of telling a `--resume-from` agent to check out the old branch.

```bash
klaus launch --change nxvp "Handle the review notes"                 # a valley handle
klaus launch --change 20261013-0900-1a2b3c4d "Fix the failing check"  # a run on the change
klaus launch --change nxvp --no-replay "Start over on the tests"      # fresh agent, same branch
```

`<ref>` names the change, resolved in this order:

1. A run ID names that run's change.
2. A full change ID or a handle is matched against the runs recorded in any session for the target repo: a run matches when the ref is its change ID, starts its change ID (a handle is the change ID's shortest unique prefix, so a handle that has since lengthened still matches), or is the valley handle recorded on it. A run is in the target repo only when its repo, named as [for the dashboard](#follow-up-runs-on-a-change), is the last component of the target repo: a run whose valley named its repo `widget` is in `widget`, not in the `acme/source` it was launched against. A launch whose target repo is unknown is an error.
3. When no run matches, and the repo delivers to a valley that lists `changes`, `valley changes --json` names the change: one whose ID or handle the ref is, or whose ID it starts.

A ref that names more than one change is an error that lists them; name it by its full change ID or a run ID. `--change` refuses a GitHub repo (use `--pr`), and can't be combined with `--pr` or `--branch`. Like `--pr`, it refuses while another run is still working on the change or its branch, unless you pass `--force-takeover` (see [A run still working on the branch](#a-run-still-working-on-the-branch)).

The worktree tracks the change's branch on origin, as `--pr` tracks the PR's: the branch of the change's active version when the valley lists one (it may have been marked ready from another machine), else the branch the change's newest run pushed. Launch fails, naming the change and its last known head, when no branch is known or the branch is gone from origin. Then:

- The run keeps the change ID; launch prints `change: <id> (inherited from change <handle or id>)`.
- In a valley's repo, launch returns the change to draft before the agent starts, as for any [follow-up run](#follow-up-runs-on-a-change).
- The agent's conversation continues from the change's most recent run, by the [trajectory replay](#trajectory-replay) rules of `--pr`: `--replay`, `--no-replay`, the size threshold and the fallback to a fresh agent all apply. A change has no budget-paused label, so replay doesn't wait for one. Launch prints the `replay:` line.
- `--change` works with every worker backend, and from a coordinator of any backend. Every backend keeps the change's branch, its change ID and the `Change-Id`/`Klaus-Run` trailers, and finalize marks its pushed head ready. Only the conversation differs: Claude replays the stored conversation, on any machine that has the data ref; Codex continues its own session on the same machine; agy starts fresh.
- The worker prompt tells the agent it is continuing the change, to commit on top of its branch and push fast-forward to it, and not to create a new branch, rebase or force-push. Finalize's `valley ready` then marks the new head ready as a version of the same change.
- On the dashboard the run joins the change's row, like a fix agent on its PR's row.

With `--resume-from`, that run's conversation is used instead of replay, and `--change` still picks the branch and the change ID. When that conversation can't be resumed (its transcript is missing or can't be staged), the agent starts fresh rather than replaying another run's, and the `replay:` line says so: `replay: fresh agent (--resume-from <run-id>: its conversation can't be resumed)`. The same holds for `--pr`.

### Branch names and `klaus launch --branch`

Every run without `--pr` or `--change` works on a new branch named `<slug>-<tail>`. The tail is the random end of the run ID (`bd92f081` for run `20261011-1843-bd92f081`), which keeps names unique and ties each branch to its run. The slug comes from the first of these that yields one:

1. the `--issue` title, when it can be fetched: a GitHub issue's, or a [knot's](#working-a-knot-klaus-launch---issue-knot);
2. the brief's first `# ` heading, outside code fences;
3. the brief's first non-empty line.

The text loses a leading `<repo-name>:`, is lowercased, has each run of characters outside `[a-z0-9]` turned into one `-`, and is cut at a word boundary to at most 40 characters. A brief that opens with `# cosmo: the klaus-env guest starts with classic-laddie` in `cosmo` names its branch `the-klaus-env-guest-starts-with-classic-bd92f081`. When nothing yields a slug, or git rejects the name, the branch is `agent/<run-id>`.

```bash
klaus launch --branch fix/healthz-timeout --prompt-file /tmp/task.md
```

`--branch` sets the name exactly. It must pass `git check-ref-format --branch` and must not already exist locally or on origin, and it can't be combined with `--pr` or `--change`, which work on the PR's or the change's branch. A `--resume-from` follow-up gets a new branch named the same way.

### `klaus launch --prompt-file`

Read the agent's prompt from a file instead of the positional argument. The file is read verbatim, so backticks, quotes, `$VAR`, and newlines survive intact:

```bash
klaus launch --prompt-file /tmp/task.md
klaus launch --prompt-file /tmp/task.md --issue 42 --budget 10
```

Prefer this for long or technical prompts. A prompt passed as a shell argument goes through the shell first — in zsh, backticks inside a double-quoted string are command substitution, so code spans are deleted or replaced by command output before klaus sees them, and the agent gets a briefing with holes exactly where the detail was.

A prompt can be any length. However it is given, klaus writes it to `~/.klaus/sessions/<session>/prompts/<run-id>.md` (kept after the run as the record of the brief) and every worker reads it on stdin, so it never goes on the tmux command line, which tmux caps at about 16KB. A local claude worker also reads its system prompt from `prompts/<run-id>.system.md`. agy has no system prompt flag, so an agy worker reads the system prompt and the brief together on stdin, from `prompts/<run-id>.input.md`. What still goes inline (the worker system prompt for codex and for sandbox claude runs) is checked up front: if the pane command would exceed 12000 bytes, launch fails before creating a worktree and names the part that is too large.

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

After the runs, `klaus status` lists the session's jobs (see [Consulting other models](#consulting-other-models)): every one running and those that finished in the last 3 hours, with their ID, kind, thread or PR, backend/model, status, how long they have run or took, and prompt. The table is left out when there are none.

The status dashboard shows these columns for each run:

| Column | Values | Meaning |
|--------|--------|---------|
| STATUS | `running` / `needs-attention` / `pr-created` / `merged` / `closed` / `no-access` / `awaiting-integration` / `integrated` / `branch-gone` / `awaiting-approval` / `approved` / `uncertain` / `stale` / `rejected` / `failed` / `withdrawn` / `superseded` / `draft` / `working` / `exited` | Where the run stands. `running`: the agent's pane is running it, as the dashboard judges it (a dead pane, or an idle one once the run finished, isn't). `working`: another run of its change is running (see [Follow-up runs on a change](#follow-up-runs-on-a-change)). `no-access`: GitHub won't show the PR with the current credentials, and the other PR columns aren't queried. `awaiting-integration`, `integrated` and `branch-gone` apply to runs that pushed a branch without a PR (see [Runs that end without a PR](#runs-that-end-without-a-pr)); the rest apply to runs whose change is on a valley (see [Repos that deliver to a valley](#repos-that-deliver-to-a-valley)) |
| RUN ID | run ID | The run's ID |
| CHANGE | `#N` / handle / change ID prefix / `-` | The change the run belongs to: its PR's number, else its valley change's handle, else the first 8 characters of its change ID, as the dashboard names the change's row. Every run of a change shows the same handle, running or not: the valley listing's, else one a run of the change recorded at ready or unready |
| PROMPT | text | The prompt, or the title of the run's change on a valley, cut at 40 characters |
| CI | `passing` / `failing` / `pending` / `unknown` | CI check status for the PR |
| CONFLICTS | `none` / `yes` / `unknown` | Whether the PR has merge conflicts |
| BEHIND | `-` / `N` | Commits the base branch has that the PR head lacks (stale-but-mergeable when >0) |
| MERGE | `ready` / `behind N` / `blocked` / `pending` | Overall merge readiness (combines CI, conflicts, behind-ness, and review status) |

### `klaus dashboard`

Live TUI view of the PR pipeline. Auto-refreshes via filesystem watching and GitHub polling every 30s.

Runs are listed by what they work on: the runs on a PR share its row, and the runs without a PR on one change (the same change ID in the same repo) share the change's row; a run with neither has a row of its own. Rows are grouped by whose move it is, most recent first within each group:

| Group | What's in it |
|-------|--------------|
| **yours** | runs needing attention (`ATTN`: salvaged work, stalled or budget-paused PRs); valley changes to approve (`READY v<n>@<short head>`) or to look at (`STALE`, `REJECTED`, `FAILED`, `UNCERTAIN`, `DRAFT`); pushed branches to submit (`SUBMIT <branch>@<short sha>`); open, non-draft PRs that are ready to merge and not yet approved with `klaus approve` |
| **agents** | rows with a running agent, whatever else is true of them: a fix agent on a PR, a follow-up on a change (see [Follow-up runs on a change](#follow-up-runs-on-a-change)) |
| **pipeline** | every other open PR: CI pending or failing (fix agents are dispatched automatically), conflicts, behind, approved and waiting to merge, CI unknown; valley changes approved and waiting on the integrator (`APPROVED`) |
| **done** | merged, closed, integrated (`INTEGRATED`, or `INTEGRATED~` for a content match), gone (`GONE`), withdrawn (`WITHDRAWN`), no access (`NO ACCESS`), and exited agents |

The line above the list counts each group, starting with yours (e.g. `2 yours · 3 agents · 5 pipeline · 12 done`), and shows the position (e.g. `12–30 of 34`). A repo column appears when the session spans more than one repo or lists a valley change. A change's row is built like a PR's: it shows the valley's handle, else the first 8 characters of the change ID, in place of `#<n>`; the valley's title, else the subject of the head its latest run pushed, in place of the PR's title; and where the valley says the change's active version stands, with that version's head to approve, wherever it was marked ready; for a change no valley lists, where its latest push stands (see [Repos that deliver to a valley](#repos-that-deliver-to-a-valley)). Earlier versions fold into the row rather than showing as `SUPERSEDED`. A newer push the valley doesn't hold as a version, as when its ready failed, doesn't take the row over: the footer notes it (`a newer push 1a2b3c4 isn't ready`). A valley change's row always shows its repo. While an agent runs on a PR or a change, the row names it, and the footer says which agent is working on it. The list scrolls when it doesn't fit the pane. The selection stays on its row as runs reorder, and is kept in view.

The dashboard checks pushed branches every minute in the background, independently of GitHub polling, with at most one `valley changes` per check (see [Runs that end without a PR](#runs-that-end-without-a-pr)). A PR the agent can no longer read on GitHub (a 404, a 403 that isn't a rate limit, or "Could not resolve to a PullRequest/Repository") shows `NO ACCESS` and is done; the dashboard skips its CI, conflict and review queries and asks again every 30 minutes, so a fixed credential recovers on its own.

Done rows older than an hour are hidden by default; the position line says how many (e.g. `21 hidden`), and `h` toggles showing them. Exited agents count only once their worktree is cleaned up. A row the dashboard watched finish (a PR it saw merge, a branch it saw as `SUBMIT` integrate) stays for the full hour after it finished. Rows in yours, agents and pipeline are never hidden. Set the threshold with `"dashboard": {"hide_finished_after_minutes": 60}` in config; a negative value never hides anything.

Set `"dashboard": {"approve_command": "cd ~/hack/{repo} && valley approve {handle} --head {head}"}` and the footer shows the expanded command (`approve: …`) whenever a `READY` row is selected, and `submit: …` for a `SUBMIT` row. `{repo}` is the repo as the row names it (the valley's name for a valley change, else the short project name), `{branch}` the pushed branch, `{change}` the full change ID, `{handle}` the change's handle and `{head}` the pushed head, so the approval binds the head you saw. Each value is shell-quoted unless it is only letters, digits and `._/-`, so a branch name can't inject commands; don't put quotes around placeholders in the template. A row with no value for a placeholder the template uses, such as `{handle}` on a branch no valley knows, shows no command, and neither does a `DRAFT` row or one with an agent running on it. It's there to copy and run on your own machine; the dashboard never runs it. `submit_command` is the deprecated name of `approve_command` and is read only when `approve_command` is unset.

Below the run rows, a jobs section lists the session's consults and reviews, as `klaus status` does: every running job (`RUNNING`, with how long it has run), and those that finished in the last 3 hours (`SUCCEEDED`, `FAILED`, `TIMED OUT`, `INTERRUPTED`, or `DIED` when the job's klaus was killed without recording its end). Jobs don't join the change and PR rows. The section follows the job records through the same file watcher as the runs, and re-reads them every 30 seconds, which also notices a job that died. Press `tab` to select a job: the footer says how it stands, with its error and where its raw output was saved, and `enter` opens its record and answer as `klaus jobs show` prints them, in place of the list (`j`/`k` scroll, `esc` goes back).

On a narrow or short pane the layout degrades rather than wrapping: the prompt column shrinks and long lines are cut at the edge, then spacing, status, job and help lines are dropped, before any rows are.

Keyboard shortcuts:

| Key | Action |
|-----|--------|
| `j` / `k` (or `↑` / `↓`) | move the selection |
| `PgUp` / `PgDn` | move a page |
| `g` / `G` (or `Home` / `End`) | jump to the top / bottom |
| `h` | show all runs / hide old done rows |
| `a` | approve the selected PR |
| `d` | discuss the selected PR with the coordinator (pre-fills `WRT PR#<num>:` in the session pane and switches focus there) |
| `o` | open the selected PR in a browser |
| `tab` | select a job in the jobs section / go back to the runs |
| `enter` | read the selected job's record and answer (`esc` goes back) |
| `r` | force refresh |
| `q` | quit |

The dashboard follows klaus on PATH. Every 30 seconds it resolves `klaus` on PATH, symlinks and all, and compares it with the binary it runs. When PATH names a different file that runs `--version`, as after `klaus self-update`, the footer says `klaus updated to <version>; restarting`, and once no key has been pressed for 10 seconds the dashboard shuts down as on `q`, closing its watcher, its log and its webhook server, and execs the new klaus with the same arguments and environment. The restarted dashboard rebuilds its view from the state on disk. If the exec fails, the dashboard keeps running on the old klaus, shows the error in the footer, and doesn't try that file again.

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
  "push_data_ref": false,
  "default_branch": "main",
  "trusted_reviewers": ["gemini-code-assist[bot]"],
  "pr_reviewer": "octocat",
  "require_approval": true,
  "auto_merge_on_approval": false,
  "merge_verify_command": "nix build",
  "default_agent_model": "claude-sonnet-5",
  "default_agent_effort": "medium",
  "agent_display": "detached",
  "dashboard": {"hide_finished_after_minutes": 60, "approve_command": "cd ~/hack/{repo} && valley approve {handle} --head {head}"}
}
```

`push_data_ref` controls whether finalize pushes `data_ref` to origin. It defaults to `false`: each run's state, log and Claude conversation are committed to the data ref in the local clone, where trajectory replay reads them, and nothing is pushed. Set it to `true` (in `~/.klaus/config.json` or the target repo's `.klaus/config.json`) to push the ref after every run, which lets replay work across machines. Agent transcripts contain prompts, file contents and environment details, and a pushed data ref is readable by anyone who can read the repo, so leave it off for public repos. A push sends the ref's whole history, including runs committed while pushing was off. `klaus push-log <id> --push` pushes once without changing the setting.

`trusted_reviewers` lists GitHub logins whose comments dispatch a fix agent even without a formal "changes requested" review. Both inline review comments and PR conversation comments count. A comment stops counting once a newer commit is pushed; editing a comment counts as new, so feedback added to an older comment after the latest push is picked up.

Conversation comments by the PR author are the exception: the operator and fix agents post through the same GitHub account, so an author's conversation comment is ignored unless it opts in, even when the author is in `trusted_reviewers`. Opt in either by starting the comment with a `/klaus fix` line, or by putting the hidden marker `<!-- klaus-actionable -->` on a line of its own. Quoted lines (`> /klaus fix`) don't count. Trusted reviewers other than the PR author need no opt-in, and inline review comments are unaffected by this rule.

`pr_reviewer` is the GitHub login a launched agent requests review from when it opens its PR (`gh pr create --reviewer`). Klaus uses it as given, without asking GitHub. When it is unset, klaus picks the reviewer at launch. It lists the target repo's collaborators with push access, then leaves out bots and the `gh` user the agents run as, since that user authors their PRs and GitHub ignores a review request aimed at a PR's author. If the repo's owner is still on the list, the owner is the reviewer; otherwise, if exactly one user is left, that user is. If several users are left, or a `gh` call fails, launch prints a one-line warning naming the candidates or the error, and the agent opens its PR without requesting a review. Runs that don't open a new PR (`--pr` fixes and [direct-push repos](#runs-that-end-without-a-pr)) skip the lookup.

`merge_verify_command` runs in the rebased worktree during `klaus merge` to sanity-check the branch before force-push. When unset, klaus runs `go build ./...` only if a `go.mod` is present, otherwise skips the check (non-Go repos rely on CI).

`default_agent_model` / `default_agent_effort` set the `claude --model` / `--effort` for launched agents when the launch doesn't pass the corresponding flag. When unset, the flag is omitted from the `claude` command entirely.

`dashboard.hide_finished_after_minutes` sets how long done rows (merged, closed, integrated, gone, no access, cleaned up) stay on the dashboard before they're hidden behind `h` (default 60; negative never hides them).

`dashboard.approve_command` is a template for the command that approves a valley change, shown in the dashboard footer when a `READY` row (or a `SUBMIT` row) is selected. `{repo}` expands to the repo as the row names it, `{branch}` to the pushed branch, `{change}` to the full change ID, `{handle}` to the change's handle and `{head}` to the pushed head, each shell-quoted unless it is only letters, digits and `._/-` (so leave placeholders unquoted). A row with no value for a placeholder the template uses shows no command. It's empty by default, and klaus never runs it. `dashboard.submit_command` is its deprecated name, read only when `approve_command` is unset.

`agent_display` controls where agent panes live. `detached` (the default) puts each agent in its own window of a detached tmux session named `klaus-agents-<session-id>`, so your coordinator window is never split. `pane` restores the old behaviour of splitting the coordinator's window for every agent.

**`.klaus/prompt.md`** — Custom system prompt for launched agents. Go template variables: `{{.RunID}}`, `{{.Issue}}`, `{{.Branch}}`, `{{.RepoName}}`, `{{.Reviewer}}`, `{{.DirectPush}}` (true when the repo's `origin` is not on github.com; see [Runs that end without a PR](#runs-that-end-without-a-pr)). Customize this to match your repo's conventions, test commands, and PR workflow. Klaus appends a short "Non-interactive run" section to every worker prompt, custom or default (including `pr-fix-prompt.md`), so agents know their run ends with their turn.

**`.klaus/session-prompt.md`** — Custom prompt for the coordinator session. Same template variables.

**`.klaus/pr-fix-prompt.md`** — Custom prompt for PR-fix agents. Additional variable: `{{.PR}}`.

## Under the hood

- **Agents run headless** — `claude -p`, `codex exec` and agy's print mode read the prompt on stdin from its
  file, so there is no way to type at a running agent. Correcting one mid-run means relaunching with
  `klaus launch --resume-from <run-id>`; see [docs/AGENT_MESSAGING.md](docs/AGENT_MESSAGING.md)
  for why in-place messaging isn't offered and what it would take. The run ends when the agent's turn ends, so every worker prompt
  tells the agent to run long builds and tests in the foreground and never end its turn with background work still running
- **Worktrees** isolate each agent — they can't step on each other or your working tree
- **tmux panes** manage each agent's process lifecycle. They live in a detached `klaus-agents-<session-id>` session owned by the tmux server, so they cost you no screen space and agents survive the coordinator exiting (`agent_display: "pane"` splits your window instead)
- **Workers can't drive that tmux server.** A local worker starts with `$TMUX` and `$TMUX_PANE` removed and `TMUX_TMPDIR` set to its run's own empty directory (`~/.klaus/sessions/<session>/tmux/<run-id>`, mode 0700). A `tmux` command the worker or its tools run, even `tmux kill-server`, talks to that empty location, not to the server running your session, the coordinator and every agent's pane. klaus's own commands inside the worker (`klaus _pre-review`, `klaus status`) reach the real server through `KLAUS_TMUX_SOCKET`, its socket path recorded at launch; `_finalize` runs after the worker in the pane's own shell. tmux falls back to `/tmp` when `TMUX_TMPDIR` names a directory that doesn't exist, so a worker that points it at a missing directory can still reach a server on tmux's default socket
- **JSONL logs** are saved for replay and post-run analysis
- **Sensitivity scanning** checks logs for private IPs, SSH keys, and credentials before persisting (the run repo's own remote hosts are exempt; a log that cannot be scanned in full is kept off the data ref)
- **State storage** — session state lives in `~/.klaus/sessions/` (ephemeral, machine-local), while finalized run artifacts are committed to the local clone's data ref
- **Data ref** (`refs/klaus/data`) stores run metadata without polluting your branch list. It is pushed to origin only when `push_data_ref` is true

## Requirements

- `tmux` (sessions run inside tmux)
- At least one configured backend CLI: `claude`, `codex`, or `agy` (authenticated)
- `git` 2.30 or later (needed for agent worktrees; sessions can run without it)
- `gh` (GitHub CLI, for PR operations)

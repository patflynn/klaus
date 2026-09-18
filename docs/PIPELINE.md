# Klaus: From Idea to Merged PR

This guide walks through how work flows through klaus — from research in the
coordinator session to a merged PR on main. For command reference see
[README.md](../README.md); for architecture details see [DESIGN.md](DESIGN.md).

## The big picture

```
 Coordinator (you + Claude)
      │
      │ research, plan, write prompts
      │
      ▼
 klaus launch ──────────────────────────────┐
      │                                     │
      ├─► Agent 1 (worktree + tmux pane)    │
      ├─► Agent 2                           │ each pushes a branch
      └─► Agent 3                           │ and opens a PR
                                            │
                                            ▼
                                     Dashboard pipeline
                                     watches CI, reviews,
                                     conflicts — dispatches
                                     fix agents automatically
                                            │
                                            ▼
                                     klaus merge / auto-merge
                                            │
                                            ▼
                                         main
```

---

## 1. Planning & Research (Coordinator)

When you run `klaus`, you get an interactive Claude Code session — the
**coordinator**. This is where all the thinking happens. The coordinator reads
files, greps code, checks GitHub issues, and builds a mental model of the work.

The coordinator's job is to produce **rich agent prompts**. Agents have access to
the repo but zero conversation history — the prompt is their entire briefing.

**Bad prompt** — the agent flounders:

```
klaus launch "fix the auth bug"
```

**Good prompt** — the agent knows exactly where to look:

```
klaus launch --issue 42 "The JWT validation in internal/auth/verify.go:ValidateToken()
silently accepts expired tokens because the time comparison on line 87 uses Before()
instead of After(). Fix the comparison and add a test in internal/auth/verify_test.go
that confirms expired tokens are rejected. See issue #42 for the user report."
```

A good prompt includes:
1. **What** needs to change and **why**
2. **Specific file paths and function names**
3. **Acceptance criteria** — what "done" looks like
4. **Constraints** — e.g., "don't change the public API"

## 2. Agent Execution

When the coordinator runs `klaus launch`, several things happen in sequence:

1. **Worktree creation** — `git worktree add` creates an isolated copy of the
   repo branched from `origin/main`. The agent can't interfere with other agents
   or your working tree.

2. **Tmux pane** — a new pane splits off in your terminal, showing the agent's
   live output.

3. **Claude runs autonomously** — the agent runs Claude Code with
   `--dangerously-skip-permissions` and `--output-format stream-json`, piped
   through a processing chain:

```
claude (JSONL) ──► tee (save log) ──► _format-stream (live display)
                                              │
                                        agent exits
                                              │
                                              ▼
                                      _finalize <run-id>
                                     (extract cost, duration, PR URL)
```

4. **Push & PR** — the agent commits, pushes its branch, and creates a PR via
   `gh pr create`.

5. **Finalization** — `_finalize` parses the JSONL log to extract cost, duration,
   and PR URL, then updates the run state file. It also cleans up the worktree
   (the state and logs persist).

For PR fixes (`--pr`), the agent checks out the existing PR branch instead of
creating a new one. It pushes directly — the PR updates automatically.

## 3. Pipeline Automation (Dashboard)

Run `klaus dashboard` to open a live TUI that monitors all active PRs. The
dashboard combines two event sources:

- **Filesystem watching** (fsnotify) — detects state file changes instantly
- **GitHub polling** — fetches CI, conflict, and review status every 30 seconds

The pipeline controller evaluates each PR on every poll and advances it through
a state machine:

```
                    ┌──────────────┐
             ┌─────►  ci_pending   ◄─────────────────────┐
             │      └──────┬───────┘                      │
             │             │                              │
             │    ┌────────┴────────┐                     │
             │    ▼                 ▼                     │
             │  ci_failed       ci_passed                 │
             │    │                │                      │
             │    │       ┌────────┴────────┐             │
             │    │       ▼                 ▼             │
             │    │  review_pending      approved         │
             │    │       │                │              │
             │    │       │         ┌──────┴──────┐       │
             │    │       │         ▼             ▼       │
             │    │       │    needs_rebase     merge ──► merged
             │    │       │         │
             │    │       │    dispatch rebase
             │    │       │         │
             │    │       │    ┌────┴────┐
             │    │       │    ▼         │
             │    │       │  stalled     │
             │    │       │  (retries    │
             │    │       │  exhausted)  │
             │    │       │             success
             │    ▼       ▼              │
             │  dispatch fix agent       │
             │    │       │              │
             │    └───┬───┘              │
             │        │ (agent pushes)   │
             └────────┴──────────────────┘
```

Key behaviors:

- **CI failure** — the controller dispatches a fix agent (`klaus launch --pr`)
  with a prompt that tells it to check `gh pr checks` and `gh run view --log-failed`.

- **Changes requested** — if a reviewer (or a trusted bot like
  `gemini-code-assist[bot]`) requests changes, the controller dispatches an agent
  to address the comments. After the agent completes, unresolved review threads
  are automatically marked as resolved.

- **Merge conflicts** — when a PR has conflicts with passing CI, a rebase agent
  is dispatched to rebase onto main, resolve conflicts, run tests, and push.
  Approval is not required; the rebase circuit breaker stops after
  `maxFixAttempts` consecutive failed rebase attempts.

- **Retry logic** — if an agent dispatch fails, the controller retries up to 2
  times with 60-second backoff. A 60-second cooldown prevents duplicate dispatches
  for the same PR. After retries are exhausted, the PR moves to `stalled` and
  needs human attention.

- **Auto-merge** — when `auto_merge_on_approval` is enabled in config and a PR
  is approved with passing CI and no conflicts, the controller merges it
  automatically (calls `klaus merge --yes`). Disabled by default.

- **Behind the base branch** — an approved PR that is conflict-free but behind
  its base is updated first (`gh pr update-branch`), which satisfies an
  up-to-date-branch protection rule; the merge happens on a later poll once
  GitHub reports the PR caught up and CI has re-run.

- **Auto-merge retries** — merge attempts and branch updates each get their own
  budget of 3 tries per approval, spaced at least 60 seconds apart, so a merge
  that cannot succeed is not retried on every poll. When a budget is spent the
  PR moves to `stalled` and a single `pipeline:stalled` event is emitted with
  the reason. Losing and regaining approval resets both budgets.

- **Event edges** — `pr:approved` is emitted once per approval, not once per
  poll: a failed merge moves the PR out of `approved` and back again, which
  would otherwise flood `klaus watch` with duplicate notifications.

## 4. Review & Approval

Klaus distinguishes between GitHub review approval and internal approval:

- **GitHub approval** — when a reviewer approves the PR on GitHub, the pipeline
  detects it and marks the internal run state as approved too.

- **Trusted reviewers** — configured in `.klaus/config.json` under
  `trusted_reviewers`. When a trusted reviewer leaves comments (even without
  formal "changes requested"), the pipeline dispatches a fix agent. Both
  inline review comments and PR conversation comments (the top-level thread
  on the PR) count. A comment counts as addressed once a commit is pushed
  after it. Conversation comments are compared by their last-edited time, so
  a reviewer editing an older comment after the latest push to add feedback
  is picked up.

  **PR-author conversation comments are opt-in.** The operator and fix agents
  post through the same GitHub account, so a conversation comment by the PR
  author is ignored, even if the author is in `trusted_reviewers`, unless it
  opts in in one of two ways:
  - its first non-blank line is a `/klaus fix` command (optionally followed by
    text), or
  - it contains the hidden marker `<!-- klaus-actionable -->` on a line of its
    own.

  Quoted lines (`> /klaus fix`) don't opt in, so a reply quoting the original
  stays inert. Conversation comments from trusted reviewers who are not the PR
  author count without opting in. Fix-agent replies are also tagged with a
  hidden `<!-- klaus-agent-reply -->` marker and ignored, but correctness
  doesn't rest on it: an agent reply that loses its marker is still an
  author comment without opt-in. Inline review comments don't apply the author
  rule; they count by `trusted_reviewers` membership and the agent-reply
  marker as before.

  In webhook mode, the relay must forward `pull_request_review` (inline
  review comments) and `issue_comment` (conversation comments, created or
  edited) events for these to be picked up without waiting for the reconcile
  heartbeat.

- **Cross-model review** — `klaus review <pr> --post` has a model family other
  than the PR author's review the diff and submits one `COMMENT` review (never
  an approval). Its body ends with a hidden marker line
  `<!-- klaus-cross-review backend=<kind> model=<model> sha=<head-sha> -->`
  (`pipeline.CrossReviewMarker`). The operator's gh account posts it, and that
  account need not be in `trusted_reviewers`, so a review carrying the marker
  counts as trusted when its author is the authenticated gh user. The marker
  alone grants nothing, so copying it into someone else's review has no effect.
  From there it is an ordinary trusted review: only reviews with inline
  comments dispatch a fix agent, a push after the review marks it addressed,
  and the review-fix circuit breaker stalls the PR if fixes keep failing.
  Findings that don't land on a diff line go in the body for the operator.
  `klaus review` itself refuses to post twice for the same reviewer and head
  commit or beyond `cross_review.max_rounds` (default 2), counting only marker
  reviews by the operator's account, so review → fix → review cannot spin.
  Within a session, a lock file per PR (`locks/review-<owner>-<repo>-<n>.lock`)
  covers the whole check → review → post span. The check runs again just
  before posting, along with a check that the PR head hasn't moved; stale
  findings posted after a push would otherwise read as unaddressed. The pipeline does not dispatch cross-reviews on its own
  yet; `review.RunPRReview` is the hook for that.

- **`klaus approve`** — marks PRs as ready for merge in the internal state. PR approval
  is strictly a human task — it exists only to ensure that all changes are gated
  by operator approval before merging. The coordinator session must never approve PRs.

- **`require_approval`** — when `true` (the default), `klaus merge` won't merge
  a PR unless it's been approved via `klaus approve` or GitHub review. Set to
  `false` to skip this gate.

- **`auto_merge_on_approval`** — when `true`, the pipeline merges PRs
  automatically as soon as they're approved with passing CI and no conflicts.
  Defaults to `false`.

## 5. Merge

`klaus merge` handles the mechanics of getting PRs into main. It processes PRs
sequentially — important because each merge can create conflicts for the next.

For each PR, the merge flow:

1. **Check readiness** — CI status, conflicts, behind-count, review decision
2. **Rebase if needed** — when the PR conflicts *or* is behind the base branch:
   fetch main, create a temp worktree, rebase the PR branch, run the merge
   verification (`merge_verify_command` if set, else `go build ./...` when a
   `go.mod` exists, else skip), force-push with `--force-with-lease`
3. **Wait for CI** — poll every 30s, up to a 10-minute timeout
4. **Merge** — via `gh pr merge` (default: squash)
5. **Update state** — mark the run as merged so the dashboard reflects it

If any PR in the queue fails (rebase conflict, CI timeout, changes requested),
merging stops and reports the stuck PR plus remaining unmerged PRs.

## 6. Cleanup & Artifacts

After an agent completes:

- **Worktrees** are removed automatically during finalization. The local
  branch is deleted only when it has no commits beyond the default branch or
  `origin`'s live tip matches it (the PR branch on the remote stays). Runs without a PR get a WIP commit and push first and
  emit `agent:needs-attention`.

- **State files** persist at `~/.klaus/sessions/{session-id}/runs/{run-id}.json`
  — these power the dashboard and contain cost, duration, PR URL, and approval
  status.

- **Data ref sync** — state and logs are committed to `refs/klaus/data` (a
  custom git ref that doesn't show up as a branch). This is pushed to the remote
  so run history travels with the repo.

- **Sensitivity scanning** — before syncing logs to the data ref, a scanner
  checks for private IPs, SSH keys, credential patterns, and `.age` secret
  references. If anything is found, the log is held back and only the state file
  is synced. Use `klaus push-log <id>` to force-push a held log after review.

## Putting It Together

Here's a realistic multi-agent session fixing three issues:

```
$ klaus
# You're now in an interactive Claude Code coordinator session

You: We need to tackle issues #31 (broken pagination), #32 (add dark mode),
     and #35 (update API docs). Let me research each one.

Claude: [reads internal/api/list.go, internal/ui/theme.go, docs/api.md]
        [checks issue threads for context]

        I've researched all three. Let me launch agents:

        [runs: klaus launch --issue 31 "The pagination in internal/api/list.go:ListItems()
        returns duplicate items when page size doesn't evenly divide the total count.
        The off-by-one is on line 42 — the offset calculation should use (page-1)*size
        not page*size. Add a test with 7 items and page size 3. See #31."]

        [runs: klaus launch --issue 32 "Add dark mode support to the settings page.
        The theme system is in internal/ui/theme.go — add a Dark theme alongside the
        existing Light theme. Wire it to a toggle in internal/ui/settings.go:renderSettings().
        Follow the existing pattern in theme.go. Add a Playwright e2e test."]

        [runs: klaus launch --issue 35 "Update docs/api.md to document the new
        /v2/search endpoint added in PR #28. Include request/response examples.
        The handler is in internal/api/search.go:HandleSearch()."]

# Three tmux panes appear. Meanwhile, the dashboard is running:
# PR #40 (issue 31) — ci_pending → ci_passed → approved (reviewer approves)
# PR #41 (issue 32) — ci_pending → ci_failed → fix agent dispatched → ci_passed
# PR #42 (issue 35) — ci_pending → ci_passed

You: Looks like #40 and #42 are ready. Let me review #41's fix.
     [reads the agent's changes, looks good]

You: klaus approve 40 41 42
You: klaus merge 40 41 42

# Merges PRs sequentially, rebasing as needed between each merge.
# All three land on main.
```

The key insight: the coordinator does the thinking, agents do the building, and
the pipeline handles the mechanical work of CI monitoring, review response, and
conflict resolution. The human operator provides final approval to gate all changes.

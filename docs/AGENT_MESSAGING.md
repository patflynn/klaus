# Messaging a running agent

**Status: not implemented.** This document records why, with the evidence.

The goal was a `klaus message <run-id> <text>` command that injects text as user
input into a running agent's Claude Code session, so a coordinator can correct
an agent mid-run instead of killing it and relaunching. The proposed mechanism
was tmux pane injection — `load-buffer` + `paste-buffer` into the agent's
`tmux_pane`, then a separate `Enter` to submit.

The acceptance criterion was that the message must land in the run's recorded
trajectory as user input. It does not. Pane injection into a klaus agent is
inert: the text never reaches the session at all.

## Root cause

Klaus agents are not interactive. `buildClaudeCommand` in
[internal/cmd/launch.go](../internal/cmd/launch.go) always launches:

```
claude -p -n <run-id> --dangerously-skip-permissions --verbose \
  --output-format stream-json --max-budget-usd <budget> \
  --append-system-prompt <sys> <prompt>
```

`-p` is print mode with the default `--input-format text`, and the prompt comes
from argv. In that configuration `claude` never reads stdin, so anything typed
or pasted into the pane's tty is discarded. There is no TUI in the pane to
receive a paste — only a pipeline whose stdout is a pipe into
`tee | klaus _format-stream`.

Coordinator sessions are the opposite: [internal/cmd/session.go](../internal/cmd/session.go)
runs `claude` interactively with `Stdin = os.Stdin`. Pane injection works there
(see experiment 3). That asymmetry is the whole finding — the technique is
sound, the agent launch mode is what blocks it.

## Experiments

All three ran against the real `claude` binary (2.1.220) in an isolated tmux
server. `scripts/repro-agent-message-injection.sh` reproduces experiments 1
and 3.

### 1. Pane injection into a headless agent — fails

Ran `claude -p --output-format stream-json` in a tmux pane on a prompt that kept
the agent busy, then pasted a two-line marker and pressed Enter while it worked.

- Marker occurrences in the recorded stream-json log: **0**
- The agent completed its turn and emitted `result` without ever seeing it.

This is exactly how klaus launches agents, so it is the case that matters.

### 2. `--input-format stream-json` over stdin — works

Same headless print mode, but with `--input-format stream-json` and stdin
attached to a FIFO. Wrote the initial prompt as a JSON user message, then wrote
a second two-line message into the FIFO while the agent was mid-tool-call.

- The agent received it and echoed it back verbatim, newline intact — it
  arrived as **one** message, not two turns.
- It is recorded in the session transcript
  (`~/.claude/projects/<cwd>/<uuid>.jsonl`) as an `attachment` entry of type
  `queued_command` carrying the full multi-line prompt.
- It is **not** echoed into claude's stdout stream-json — so it would not appear
  in klaus' own `log_file`, only in the transcript klaus stores on
  `refs/klaus/data` for replay.
- `claude` still exits at the end of the turn even with the FIFO writer held
  open. The injection window is "while the agent is working", which is the
  window the feature wants, but a message sent after `result` is lost.

### 3. Pane injection into an interactive session — works

Ran `claude` interactively (no `-p`) in a tmux pane, then
`load-buffer` + `paste-buffer -p` + `send-keys Enter` while it was mid-tool-call.

- The two-line paste landed in the input box as **one** message, both lines
  together, and submitted as a single queued message.
- Recorded in the transcript as `queue-operation` entries plus an `attachment`
  of type `queued_command` with the full multi-line text. Provenance preserved.
- Caveat: injection is TUI-state-dependent. The first attempt was swallowed by
  the trust-folder dialog, which consumed the paste and the Enter as a menu
  selection. A command doing this would need to know the pane is at a prompt.

## What would actually work

Feed the agent over stdin as stream-json (experiment 2). That means, in the
launch path:

- `buildClaudeCommand` gains `--input-format stream-json` and drops the
  positional prompt;
- each run gets a FIFO, with the initial prompt written as a JSON user message
  and a writer held open for the agent's lifetime;
- `klaus message` writes a `{"type":"user",...}` line into that run's FIFO.

This is verified to deliver a multi-line message as one message and to preserve
it in the trajectory. It was not done here because it rewrites how every agent
is launched — every run would depend on FIFO setup succeeding, and a mistake
there fails launches outright rather than degrading. That is a larger change
than the one-command scope this work was sized for, and it deserves its own
review.

Note that the trajectory record lives in the Claude session transcript, not in
klaus' `log_file`. Anything claiming complete provenance from `log_file` alone
would still be missing injected messages.

## What to do today

`klaus launch --resume-from <run-id>` already continues a prior agent's Claude
conversation — it resolves the session UUID from the previous run's log and
stages that transcript into the new worktree before invoking `claude --resume`.
Killing and relaunching therefore does not have to lose the run's reasoning,
though it does lose the in-flight turn and costs a fresh startup.

# Robustness & Maintainability Project

**Status:** Planning
**Filed:** 2026-04-09

## Motivation

Klaus has a reliability problem rooted in accidental complexity. The bug history
shows a clear pattern: most bugs aren't logic errors in isolation — they're
interaction bugs between subsystems that have grown entangled. The pipeline
controller, dashboard, tmux lifecycle, and GitHub status all share mutable state
in ways that create surprising failure modes.

Over the last ~50 commits, roughly half have been bug fixes. Recurring themes:

- **Race conditions** — pane lifecycle, process detection, async dispatch (#147, #192)
- **State consistency gaps** — webhook vs polling paths diverge; config loaded in
  multiple places; missing field propagation (#172, #184, #179)
- **Last-write-wins corruption** — individual events overwriting aggregate state (#172)
- **Async I/O blocking UI** — network calls in BubbleTea Update() (#184)
- **Environment-dependent failures** — tests break based on external state (#165)
- **Upstream API erosion** — claude CLI changes break assumptions (#187, #188)

## Approach

Two cross-cutting principles guide this work:

1. **All external systems (GitHub, tmux, git) behind interfaces.** Every
   interaction with an external binary or API goes through a defined interface,
   injected via constructors. No direct `exec.Command` calls outside the
   implementing struct. This enables testing, decouples subsystems, and
   encapsulates version-specific quirks.

2. **Separate decisions from side effects.** The pipeline controller should
   decide what to do (pure logic, testable) and return action descriptors. A
   separate executor runs them. This eliminates the class of bugs where locks
   are held during I/O and where execution failures corrupt state machine
   understanding.

## Issues — Prioritized

### P0 — Interface Abstractions

These establish the foundation that other improvements build on.

| Issue | Title | Key Concern |
|-------|-------|-------------|
| [#194](https://github.com/patflynn/klaus/issues/194) | GitHub interface | Decouple from `gh` CLI; structured errors; single fetch path |
| [#195](https://github.com/patflynn/klaus/issues/195) | Tmux / agent environment interface | Decouple from tmux binary; enable headless execution |
| [#196](https://github.com/patflynn/klaus/issues/196) | Git interface | Decouple from `git` binary; structured errors |

### P1 — Core Reliability

These fix the most common bug patterns.

| Issue | Title | Key Concern |
|-------|-------|-------------|
| [#197](https://github.com/patflynn/klaus/issues/197) | Unify webhook/polling paths | Eliminate recurring field-miss bugs |
| [#198](https://github.com/patflynn/klaus/issues/198) | Separate pipeline decide/execute | Fix mutex-during-I/O; enable pure testing |
| [#199](https://github.com/patflynn/klaus/issues/199) | Resilient agent finalization | Eliminate ghost agents from failed pipelines |

### P2 — Structural Cleanup

These improve maintainability and reduce change risk.

| Issue | Title | Key Concern |
|-------|-------|-------------|
| [#200](https://github.com/patflynn/klaus/issues/200) | Split dashboard.go | 810-line god object into focused modules |
| [#201](https://github.com/patflynn/klaus/issues/201) | Canonicalize state store | One store per session, no re-derivation |
| [#202](https://github.com/patflynn/klaus/issues/202) | Remove global mutable function pointers | Dependency injection via struct fields |

### P3 — Operational Hardening

These improve day-to-day robustness.

| Issue | Title | Key Concern |
|-------|-------|-------------|
| [#203](https://github.com/patflynn/klaus/issues/203) | Add subprocess timeouts | Prevent indefinite hangs |
| [#204](https://github.com/patflynn/klaus/issues/204) | Graceful shutdown coordination | Clean teardown of all subsystems |
| [#205](https://github.com/patflynn/klaus/issues/205) | Log cleanup errors | Observability for non-fatal failures |
| [#206](https://github.com/patflynn/klaus/issues/206) | Event-driven agent completion | Replace polling with fsnotify |
| [#207](https://github.com/patflynn/klaus/issues/207) | Table-driven pipeline FSM | Explicit transitions, easier to extend |

## Suggested Execution Order

```
#194 (GitHub interface)  ─┐
#195 (Tmux interface)     ├─→ #197 (Unify webhook/poll) ─→ #200 (Split dashboard)
#196 (Git interface)      ─┘         │
                                     ├─→ #198 (Decide/execute split)
                                     │         │
                                     │         └─→ #207 (Table-driven FSM)
                                     │
                                     └─→ #199 (Resilient finalization)

Independent (any time):
  #201 (Canonicalize store)
  #202 (Remove globals)
  #203 (Timeouts) — best done after interfaces exist
  #204 (Graceful shutdown)
  #205 (Log errors)
  #206 (Event-driven completion)
```

The interface issues (#194, #195, #196) can be done in parallel. They unblock
#197 (webhook/poll unification benefits from the GitHub interface) and #198
(decide/execute split benefits from all three). #200 (dashboard split) is
easiest after #197 simplifies the webhook handling code.

## Non-Goals

- Replacing `gh` CLI with a native HTTP client (the interface enables this
  later, but the `gh` CLI implementation is fine for now)
- Supporting non-tmux environments (the interface enables this later, but
  `TmuxEnvironment` is the only implementation needed now)
- Rewriting the dashboard in a different TUI framework

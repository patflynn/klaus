# Robustness & Maintainability Project

**Status:** In Progress
**Filed:** 2026-04-09
**Updated:** 2026-04-13

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

## Progress

### Done

| Issue | PR | Title |
|-------|-----|-------|
| [#194](https://github.com/patflynn/klaus/issues/194) | #209 | GitHub interface — defined + consumers migrated |
| [#195](https://github.com/patflynn/klaus/issues/195) | #212 | Tmux interface — defined + consumers migrated (#222) |
| [#196](https://github.com/patflynn/klaus/issues/196) | #211 | Git interface — defined + consumers migrated (#220) |
| [#197](https://github.com/patflynn/klaus/issues/197) | #214 | Unify webhook/polling — webhooks as invalidation signals |
| [#198](https://github.com/patflynn/klaus/issues/198) | #215 | Pipeline decide/execute split — mutex no longer held during I/O |
| [#199](https://github.com/patflynn/klaus/issues/199) | #216 | Stale run detection and recovery |
| [#205](https://github.com/patflynn/klaus/issues/205) | #210 | Log cleanup/finalization errors |
| [#202](https://github.com/patflynn/klaus/issues/202) | #219 | Remove global mutable function pointers — use DI |
| [#217](https://github.com/patflynn/klaus/issues/217) | #220 | Thread git.Client through all consumers |
| [#218](https://github.com/patflynn/klaus/issues/218) | #222 | Thread tmux.Client through all consumers |
| [#207](https://github.com/patflynn/klaus/issues/207) | #221 | Table-driven pipeline FSM — explicit transitions |

### Remaining — Structural Cleanup

| Issue | Title | Key Concern |
|-------|-------|-------------|
| [#200](https://github.com/patflynn/klaus/issues/200) | Split dashboard.go | 810-line god object into focused modules |
| [#201](https://github.com/patflynn/klaus/issues/201) | Canonicalize state store | One store per session, no re-derivation |

### Remaining — Operational Hardening

| Issue | Title | Key Concern |
|-------|-------|-------------|
| [#203](https://github.com/patflynn/klaus/issues/203) | Add subprocess timeouts | Prevent indefinite hangs |
| [#204](https://github.com/patflynn/klaus/issues/204) | Graceful shutdown coordination | Clean teardown of all subsystems |
| [#206](https://github.com/patflynn/klaus/issues/206) | Event-driven agent completion | Replace polling with fsnotify |

### Future Direction

| Issue | Title |
|-------|-------|
| [#208](https://github.com/patflynn/klaus/issues/208) | Consider narrowing Klaus scope to PR lifecycle automation only |

## Suggested Execution Order

```
✅ #217 (git consumer migration)  ─┐
✅ #218 (tmux consumer migration)  ├─→ #200 (Split dashboard) ← NEXT
✅ #202 (Remove globals)           ─┘

Independent (any time):
  #201 (Canonicalize store)
  #203 (Timeouts)
  #204 (Graceful shutdown)
  #206 (Event-driven completion)
  ✅ #207 (Table-driven FSM)
```

## Non-Goals

- Replacing `gh` CLI with a native HTTP client (the interface enables this
  later, but the `gh` CLI implementation is fine for now)
- Supporting non-tmux environments (the interface enables this later, but
  `TmuxEnvironment` is the only implementation needed now)
- Rewriting the dashboard in a different TUI framework

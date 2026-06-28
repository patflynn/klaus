# klaus end-to-end tests

These tests drive the **real `klaus` binary** against a **real but fully
isolated** tmux server and real git repositories. They exercise behavior the
unit tests can't: actual pane creation, the `claude | tee | _format-stream;
_finalize` pipeline, worktree lifecycle, and run-state persistence.

## Running

```bash
make test-e2e
# or
go test -tags e2e ./e2e/...
```

The suite is guarded by the `e2e` build tag, so the default `go test ./...`
(and the `build-and-test` CI job) never compile or run it. Requirements:

- `tmux` and `git` on `PATH` (real, used as-is)
- `go` (the binary is built once per run by `TestMain`)

A dedicated `e2e` CI job runs the suite on `ubuntu-latest`.

## Isolation

Every test gets its own throwaway world; nothing touches your real tmux server
or `~/.klaus`:

| Concern   | How it's isolated |
|-----------|-------------------|
| tmux      | A private server on a per-test unix socket (`tmux -S <tmpdir>/tmux.sock`). klaus' own tmux calls are aimed at it via the `$TMUX` env var; the test's control queries use `tmux -S <sock>`. Killed in `t.Cleanup`. |
| state     | `HOME` is a per-test temp dir, so all `~/.klaus/sessions/...` state lands under it. |
| externals | A per-test bin dir is prepended to `PATH` and holds the klaus binary plus fake `claude`/`gh` stubs. **git and tmux stay real.** |
| git       | A sandbox repo with a **bare local `origin`**, so worktrees, branches, and pushes work with no network. |

Unique socket name + temp `HOME` per test ⇒ the tests run in parallel safely.

### The one tmux subtlety

tmux runs pane commands through the configured *default-shell*. A login shell
can rewrite `PATH` (e.g. via the Nix profile), which would hide our stubs. The
harness starts the server with `-f /dev/null` (ignore user config) and points
`default-shell` at a tiny no-profile wrapper that forces a deterministic
`PATH`/`HOME`. That makes the stubs/binary resolvable in panes on any host.

## The fake `claude` contract

After `claude` exits, `klaus _finalize <run-id>` parses the run's captured
JSONL log. The stub emits stream-json containing:

- an `assistant` event whose text holds a PR URL matching
  `https?://github\.com/[^\s"<>\]]+/pull/\d+`, and
- a `result` event with `total_cost_usd`, `duration_ms`, and `session_id`.

`_finalize` turns those into the run's `CostUSD`, `DurationMS`, and `PRURL`.
The stub also **blocks until released** (`Harness.ReleaseClaude`) so a test can
deterministically observe the live pane (title, existence, argv) before
`_finalize` tears it down. Override the emitted JSONL with
`WriteDefaultClaudeOutput`, or replace either stub entirely with `WriteStub`.

## Adding a scenario

```go
//go:build e2e

func TestSomething(t *testing.T) {
    t.Parallel()
    h := NewHarness(t)

    res := h.RunKlaus("launch", "do the thing")
    // ... assert on res, h.ReadState(id), h.ListPanes(), h.PaneTitle(id), etc.
}
```

Key `Harness` helpers (see `harness_test.go`):

- `RunKlaus(args...)` — run the real binary with the isolated env, return stdout/stderr/exit
- `ReadState(id)` / `WaitForState(id, pred, timeout)` / `RunIDs()` — inspect run state
- `ListPanes()` / `PaneExists(id)` / `PaneTitle(id)` — query the isolated tmux server
- `WaitForClaudeStart` / `ReleaseClaude` / `ClaudeArgv` / `GHArgv` — coordinate the stubs
- `SeedState(state)` — pre-seed state for read-path tests (e.g. `status`)
- `WriteStub(name, script)` / `WriteDefaultClaudeOutput(jsonl)` — customize externals

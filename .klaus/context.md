# Repository context

## Purpose
`klaus` is a multi-agent orchestrator for Claude Code. It launches autonomous coding agents in isolated `git worktree`s and `tmux` panes, monitors their PRs through a CI/review/merge pipeline, and exposes a coordinator session, dashboard, and event stream that a human or another Claude Code instance can drive. See `README.md` and `cmd/klaus/main.go` for the entry point.

## Tech stack
- Go `1.24.0` — module `github.com/patflynn/klaus` (`go.mod`).
- CLI: `github.com/spf13/cobra` v1.10.2 (`go.mod`, used throughout `internal/cmd/`).
- TUI: `github.com/charmbracelet/bubbletea` v1.3.10 + `lipgloss` v1.1.0 (`go.mod`, used by `internal/cmd/dashboard*.go`).
- Filesystem watching: `github.com/fsnotify/fsnotify` v1.9.0 (`go.mod`).
- External CLIs shelled out at runtime: `git`, `gh`, `tmux`, `claude` (see `README.md` "Requirements" and `flake.nix` devShell).
- Build/dev environment: Nix flake (`flake.nix`, `flake.lock`) provides `go`, `gopls`, `golangci-lint`, `gh`, `git`, `tmux`. `.envrc` is one line (direnv).

## Entry points
- `cmd/klaus/main.go` — single binary `klaus`; calls `internal/cmd.Execute()`.
- `internal/cmd/root.go` — Cobra root command; running `klaus` with no args delegates to `runSession` (`session.go`).
- Subcommand files in `internal/cmd/` register their own Cobra commands (e.g. `session.go`, `launch.go`, `dashboard.go`, `watch.go`, `merge.go`, `approve.go`, `project.go`, `webhook_setup.go`, `new.go`, `init.go`, `status.go`, `logs.go`, `cleanup.go`, `target.go`, `sync.go`, `pushlog.go`, `pre_review.go`, `event_cmd.go`, `track.go`, `untrack.go`, `notifications.go`, `hidden.go`). Hidden/internal helper commands live in `hidden.go`.

## Layout
- `cmd/klaus/` — `main.go` only.
- `internal/cmd/` — all Cobra command definitions and their tests.
- `internal/config/` — loads and merges `~/.klaus/config.json` and `.klaus/config.json`; renders prompt templates (`config.go`).
- `internal/run/` — `State` struct for an agent run, run store, target-repo resolution (`run.go`, `store.go`, `target.go`, `homedir_store.go`).
- `internal/git/` — git wrapper used for worktrees, branches, fetches (`git.go`, `client.go`, `exec_client.go`).
- `internal/github/` — `gh` CLI client and PR helpers (`gh_cli_client.go`, `pr_client.go`, `github.go`, `client.go`).
- `internal/tmux/` — tmux session/pane wrapper (`tmux.go`, `client.go`); 5s default timeout for local IPC.
- `internal/pipeline/` — PR pipeline FSM with `Stage` constants (`ci_pending`, `ci_failed`, `ci_passed`, `review_pending`, `approved`, `needs_rebase`, `merging`, `merged`) and transitions (`pipeline.go`, `transitions.go`).
- `internal/event/` — append-only event log feeding `klaus watch` and the dashboard (`event.go`, `log.go`).
- `internal/stream/` — parses Claude Code stream-json output (`formatter.go`).
- `internal/scan/` — sensitive-data scanner for logs before persistence (`scanner.go`).
- `internal/draft/` — budget-pause persistence: WIP commit + draft PR + `klaus:budget-paused` label (`draft.go`).
- `internal/review/` — review and lint helpers (`review.go`, `lint.go`).
- `internal/project/` — project registry (`registry.go`, `normalize.go`), persisted at `~/.klaus/projects.json`.
- `internal/projectsync/` — conservative fetch + fast-forward for registered projects; never resets or switches branches (`projectsync.go`).
- `internal/webhook/` — HTTP webhook receiver/relay glue (`server.go`).
- `internal/nix/` — sets up dev shell when a `flake.nix` is detected in a worktree (`nix.go`).
- `docs/` — `DESIGN.md`, `DEVELOPMENT.md`, `PIPELINE.md`, `PROGRESS.md`, `REQUIREMENTS.md`, `project/`.
- `scripts/` — `update-vendor-hash.sh` (Nix vendor-hash refresh).
- `.github/workflows/` — `ci.yml`, `nightly-release.yml`, `release.yml`, `notify-cosmo.yml`, `update-vendor-hash.yml`, `zizmor.yml`.
- `VERSION` — single-line version string consumed by `Makefile` and `flake.nix`.

## Build, test, run
From the `Makefile`:
- `make build` → `go build -ldflags "-X .../internal/cmd.version=$VERSION-$GIT_SHA" -o bin/klaus ./cmd/klaus/`.
- `make test` → `go test ./...`.
- `make vet` → `go vet ./...`.
- `make lint` → `golangci-lint run ./...` (requires `golangci-lint`).
- `make install` → builds and copies `bin/klaus` to `$HOME/.local/bin/klaus`.
- `make all` → vet + test + build.
- `make clean` → removes `bin/`.

Plain Go alternatives (per `README.md` "From source"):
- `go build -o ~/.local/bin/klaus ./cmd/klaus/`
- `go test ./...`

Nix users can `nix build` / `nix develop` against `flake.nix`. After bumping Go dependencies, run `scripts/update-vendor-hash.sh` to refresh `vendorHash` in `flake.nix`.

Runtime requirements (from `README.md` "Requirements"): `tmux`, `claude`, `git`, `gh` must be on PATH.

## Conventions
- Project-level rules (`CLAUDE.md`):
  - Do NOT add `Co-Authored-By` lines mentioning Claude or Anthropic in commits.
  - Do NOT mention AI in commit messages or PR descriptions.
  - Prefer integration/e2e tests that exercise the real binary; only unit-test genuinely tricky logic.
  - Run `go test ./...` before committing; PRs without tests for new code will not be merged.
  - Update Cobra help text when CLI commands/flags change; update `README.md` for user-facing behavior changes; keep code comments accurate.
- All non-`main` code lives under `internal/` — packages are not importable outside this module.
- Each Cobra subcommand is its own file under `internal/cmd/`, paired with a `_test.go` (e.g. `launch.go` + `launch_test.go`).
- External tools are invoked via `os/exec` wrappers in dedicated packages (`internal/git`, `internal/github`, `internal/tmux`) rather than from command handlers directly.
- Budget-pause state is persisted on GitHub (draft PR + `klaus:budget-paused` label), not in local process state — see the package doc comment in `internal/draft/draft.go`.
- The pipeline does not auto-dispatch additional fix/rebase agents on a PR while a `pr-fix` run is already active on it (`README.md` "The PR pipeline").

## Gotchas
- `klaus launch` must be run inside a `tmux` session (see `internal/cmd/launch.go` long help; tmux is required to host the agent's pane).
- `.gitignore` excludes `*.jsonl` and `*.log` at the repo root — be careful adding fixtures with those extensions; place them inside subdirectories or rename if they need to be tracked.
- Session state in `~/.klaus/sessions/` is ephemeral and machine-local; finalized run artifacts are written to the `refs/klaus/data` git ref (`README.md` "Under the hood").
- `klaus webhook setup` requires both `webhook.relay_url` and `webhook.secret_file` in config (`README.md` "`klaus webhook`").
- When `sandbox_host` is configured, agents run over SSH on the sandbox and the worktree is rsynced both ways; falls back to local automatically if unreachable (`README.md` "Sandbox").
- `projectsync` is intentionally read-mostly: dirty trees, detached HEADs, diverged branches, or missing upstreams are *skipped*, never coerced (see package comment in `internal/projectsync/projectsync.go`).
- After changing Go dependencies, the Nix build will fail until `scripts/update-vendor-hash.sh` is run and the new `vendorHash` is committed in `flake.nix`.

## External dependencies
- GitHub via `gh` CLI — PR creation, review, merge, labels, repo queries (`internal/github/`).
- `git` — worktrees, branches, fetch/push, the `refs/klaus/data` ref (`internal/git/`).
- `tmux` — session and pane management for the coordinator session and each agent (`internal/tmux/`).
- `claude` (Claude Code CLI) — the underlying agent that klaus orchestrates.
- Optional inbound GitHub webhooks via a user-provided relay (`webhook.relay_url`, `webhook.secret_file` in config; `internal/webhook/`).
- Optional remote sandbox host over SSH+rsync when `sandbox_host` is set (`README.md` "Sandbox").

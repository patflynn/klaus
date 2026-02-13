# Klaus: Implementation Progress

## Phase 0: Repository & Infrastructure
- [x] go.mod / go.sum
- [x] flake.nix
- [x] .github/workflows/ci.yml
- [x] Makefile
- [x] docs/REQUIREMENTS.md
- [x] docs/DESIGN.md
- [x] docs/PROGRESS.md
- [x] cmd/klaus/main.go + internal/cmd/root.go

## Phase 1: Core Pure-Logic Packages
- [x] internal/run/ — RunState, ID generation, state I/O
- [x] internal/stream/ — JSONL stream formatter
- [x] internal/scan/ — Sensitivity scanner

## Phase 2: External Command Wrappers
- [x] internal/git/ — Worktree and data ref operations
- [x] internal/tmux/ — Pane management
- [x] internal/config/ — Configuration loading

## Phase 3: CLI Commands
- [x] launch command
- [x] session command
- [x] status command
- [x] logs command
- [x] cleanup command
- [x] push-log command
- [x] init command

## Phase 4: Polish
- [x] Hidden _format-stream / _finalize subcommands
- [ ] End-to-end testing

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
- [ ] internal/run/ — RunState, ID generation, state I/O
- [ ] internal/stream/ — JSONL stream formatter
- [ ] internal/scan/ — Sensitivity scanner

## Phase 2: External Command Wrappers
- [ ] internal/git/ — Worktree and data ref operations
- [ ] internal/tmux/ — Pane management
- [ ] internal/config/ — Configuration loading

## Phase 3: CLI Commands
- [ ] launch command
- [ ] session command
- [ ] status command
- [ ] logs command
- [ ] cleanup command
- [ ] push-log command
- [ ] init command

## Phase 4: Polish
- [ ] Hidden _format-stream / _finalize subcommands
- [ ] End-to-end testing

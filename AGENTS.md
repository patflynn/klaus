# klaus: rules for agents

klaus launches coding agents in isolated worktrees and follows them to a valley change or GitHub PR (`README.md`, `docs/DEVELOPMENT.md`, `e2e/README.md`).

## Design
- Work with any git origin; reach a forge only through its package, and assume GitHub nowhere else.
- Build new behaviour for valley changes; on GitHub, make the smallest change that keeps PRs working.
- Valley changes and PRs get the same columns, labels and actions on the dashboard and in `klaus status`; call out any asymmetry in the change's description.
- Mechanics common to every author, including keeping concurrent changes coherent (ordering, restacks, conflicts between ready changes), belong in the valley as client-agnostic state or events; klaus acts where an agent or human is needed and doesn't order or time work around concurrent changes.
- klaus runs bounded agents and never holds approval or promotion keys.
- Wake on the files and refs you read, not timers; a new ticker, interval or sleep needs a stated reason.
- Delimit knot text, changed content and review notes in prompts as untrusted data, never instructions.
- Fix one-off drift once by hand with a documented step, never with a recurring sweep.

## Go
- Put non-main code in `internal/`, and Cobra commands in `internal/cmd/` (one file per subcommand).
- Call external tools (`git`, `gh`, `tmux`, `valley`) through `internal/` packages, not command handlers.
- Check compilation with `go vet ./...` or `go test ./...`; when needed, build binaries outside the tree (`$KLAUS_SCRATCH`), and never commit one.
- After Go dependency changes, commit the new `vendorHash` in `flake.nix` (`scripts/update-vendor-hash.sh`), or the Nix build fails.
- `.gitignore` ignores `*.jsonl` and `*.log` everywhere; `git add -f` such fixtures.

## Tests
Test behaviour to satisfy yourself the module works, not to mirror code or chase 100% coverage; comprehensive e2e suites at every layer explode cost.
- Give each user-visible behaviour one e2e path at the outermost layer showing it (`make test-e2e` on the real binary).
- Focus inner-layer tests on their own logic.
- Don't re-prove upstream software (`git`, `tmux`, `gh`, `cobra`).
- Count test runtime and check time in the change's cost.
- New behaviour needs a test at the right layer.
- Run `go test ./...` before pushing, and `make test-e2e` when touching behaviour the e2e suite covers.

## Writing
- Write what a reader can't infer from the code (purpose, reasons, constraints, non-obvious consequences); leave out details, boundary conditions and machinery the code already shows.
- Committed text (docs, comments) states the current design in present tense, never how it used to be.
- Update Cobra help when commands or flags change, and `README.md` for user-facing behaviour; keep comments accurate.

## Commits and PRs
- No AI attribution or Claude/Anthropic `Co-Authored-By` lines in commit messages or PR descriptions; the `Klaus-Run:` trailer klaus adds records provenance.
- Never commit to `main` directly.

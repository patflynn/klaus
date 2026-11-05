# klaus: rules for agents

klaus launches coding agents in isolated worktrees and follows their work to a valley change or a GitHub PR. `README.md` describes its behaviour, `docs/DEVELOPMENT.md` the development loop and the layout, and `e2e/README.md` the end-to-end suite.

## Design
- **Forge-agnostic.** klaus works with any git origin. Code that needs a forge reaches it through a package for that forge, and nothing else assumes GitHub.
- **Valley changes are first-class; GitHub stays minimal.** Build new behaviour for valley changes. On GitHub, make the smallest change that keeps PRs working.
- **One model of a change.** A valley change and a PR get the same columns, labels and actions on the dashboard and in `klaus status`. Call out any asymmetry in the change's description.
- **The valley does mechanics; klaus judges.** Mechanical work that is the same for every author belongs in the valley, which reports it as a client-agnostic state or event. klaus acts where the work needs an agent or a human. Keeping concurrent changes coherent (ordering, restacks, conflicts between ready changes) is the valley's job: klaus doesn't order or time work around it.
- **Bounded agent runs, no keys.** klaus runs agents. It never holds approval or promotion keys.
- **Events, not timers.** Wake on the files and refs you read. A new ticker, interval or sleep needs a stated reason.
- **Data, not instructions.** Knot text, changed content and review notes enter prompts as delimited, untrusted data.
- **One-off drift is fixed once,** by hand with a documented step, never with a recurring sweep.

## Go
- Non-main code lives under `internal/`. Cobra commands live in `internal/cmd/`, one file per subcommand.
- Call external tools (git, gh, tmux, valley) through their packages (`internal/git`, `internal/github`, `internal/tmux`, `internal/valley`), not from command handlers.
- Check that the code compiles with `go vet ./...` or `go test ./...`. When you need the binary, build it outside the tree: `go build -o "$KLAUS_SCRATCH/klaus" ./cmd/klaus`. Never commit a binary.
- After changing Go dependencies, run `scripts/update-vendor-hash.sh` and commit the new `vendorHash` in `flake.nix`, or the Nix build fails.
- `.gitignore` ignores `*.jsonl` and `*.log` everywhere, so a fixture with one of those extensions needs `git add -f`.

## Tests
Tests should ensure the behavior of the module is correct and serve the agent to satisfy themselves of that. They shouldn't be mirrors of implementations trying to get 100% code coverage. e2e tests are great, but if every layer of the onion defines its own e2e and tries to comprehensively test that layer, the cost explodes.
- Give each user-visible behaviour one end-to-end path, at the outermost layer that shows it. In klaus that is the real binary, in the e2e suite (`make test-e2e`), which drives it against a private tmux server and real git repositories.
- Give inner layers focused tests of their own logic.
- Don't write tests that re-prove upstream software: git, tmux, gh or cobra.
- A test's cost, in run time and check time, is part of the change.
- New behaviour comes with a test at the right layer.
- Run `go test ./...` before pushing, and `make test-e2e` when the change touches behaviour the e2e suite covers.

## Writing
- Committed text (docs and comments) states the current design in present tense, without narrating how it used to be.
- When a CLI command or flag changes, update its help text in the cobra command. Update `README.md` for user-facing behaviour, and keep comments accurate.

## Commits and PRs
- Commit messages and PR descriptions carry no AI attribution and no `Co-Authored-By` lines for Claude or Anthropic. The `Klaus-Run:` trailer klaus adds records provenance.
- Never commit to `main` directly.

# Developing klaus

Klaus is developed using klaus. The typical flow is a `klaus session` in the klaus repo itself, with agents doing the implementation work.

## Getting started

```bash
cd klaus
nix develop       # go, gopls, golangci-lint, gh, git, tmux
klaus session     # start a coordinator session
```

From inside the session, you and Claude plan work together, then fan out to agents:

```
You: Let's add a --model flag to the launch command. Can you
     launch an agent for that?

Claude: [runs klaus launch "Add --model flag to launch command" --issue 5]
```

A new tmux pane appears with an autonomous agent working in its own worktree. You keep talking to Claude about the next thing.

## The development loop

1. **Capture ideas as issues** — especially when they come up while using klaus on other repos:
   ```bash
   gh issue create --repo patflynn/klaus --title "Support --model flag"
   ```

2. **Start a session** and launch agents against those issues:
   ```bash
   klaus session
   # inside the session:
   # klaus launch "Fix #5: add --model flag" --issue 5
   # klaus launch "Fix #8: improve status output" --issue 8
   ```

3. **Monitor with `klaus status`** — Claude in the coordinator session can check on agents and course-correct.

4. **Review PRs** — agents push branches and open PRs. Review, merge, move on.

5. **Rebuild and pick up changes** — after merging:
   ```bash
   make install   # fast: go build to ~/.local/bin/
   ```
   Other repos pick up new versions with `nix flake lock --update-input klaus`.

## Developing klaus while using it elsewhere

Ideas for klaus improvements surface while using it on other repos. Don't context-switch — file the issue and keep working:

```bash
# Working in cosmo, notice klaus could be better
gh issue create --repo patflynn/klaus --title "Add timeout support for agents"

# Later, in the klaus repo
klaus session
# launch an agent to fix it
```

Two ways to get the binary:
- **`make install`** — fast `go build`, good for active iteration
- **`nix build .`** / flake input — reproducible, what CI and other repos use

## Project layout

```
cmd/klaus/main.go        Entry point
internal/
  cmd/                   Cobra commands (one file each)
  run/                   Run state management (pure logic)
  stream/                JSONL stream formatter (pure logic)
  scan/                  Sensitivity scanner (pure logic)
  git/                   Git worktree + data ref ops
  tmux/                  Tmux pane management
  config/                Config loading + prompt templates
```

Pure-logic packages (`run`, `stream`, `scan`) have no external deps and are fully unit-tested. External wrappers (`git`, `tmux`) are integration-tested against temp resources.

## Nix package

When Go dependencies change, update the vendor hash:
1. Set `vendorHash` to a dummy value in `flake.nix`
2. `nix build .` — the error gives you the correct hash
3. Update `vendorHash` with the real hash

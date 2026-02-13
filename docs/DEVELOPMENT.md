# Developing klaus

## Setup

```bash
cd klaus
nix develop   # drops you into a shell with go, gopls, golangci-lint, gh, git, tmux
```

Or if you prefer not to use nix, install Go 1.24+ and the tools above manually.

## Build, test, iterate

```bash
make build    # go build → bin/klaus
make test     # go test ./...
make vet      # go vet ./...
make lint     # golangci-lint (if installed)
make install  # build + copy to ~/.local/bin/
```

The full cycle:
```bash
# hack on code
make test && make install
# test manually in another tmux session
```

## Project layout

```
cmd/klaus/main.go        Entry point — just calls cmd.Execute()
internal/
  cmd/                   Cobra command wiring (one file per command)
  run/                   Run state: ID gen, save/load, list (pure logic, no I/O deps)
  stream/                JSONL stream formatter (pure logic)
  scan/                  Sensitivity scanner (pure logic)
  git/                   Git worktree + data ref operations (shells out to git)
  tmux/                  Tmux pane management (shells out to tmux)
  config/                Config loading + prompt template rendering
```

The `internal/` packages are split by dependency boundary:
- **Pure logic** (`run`, `stream`, `scan`): no external commands, fully unit-testable
- **External wrappers** (`git`, `tmux`): shell out to system commands, integration-tested
- **Config** bridges both: pure JSON/template logic, reads from filesystem

## Testing

```bash
make test                          # all tests
go test ./internal/run/ -v         # one package, verbose
go test ./internal/git/ -v -run TestWorktree   # specific test
```

Git integration tests create temporary repos in `t.TempDir()` — no fixtures, no cleanup needed.

## Nix package

The flake produces both a dev shell and a package:

```bash
nix build .              # build the package
./result/bin/klaus       # run it
nix develop              # enter dev shell
```

When you change Go dependencies, update the vendor hash:
1. Set `vendorHash` to a dummy value in `flake.nix`
2. Run `nix build .` — the error message gives you the correct hash
3. Update `vendorHash` with the real hash

## Using klaus on other repos

Add klaus as a flake input in the target repo:

```nix
inputs.klaus = {
  url = "github:patflynn/klaus";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

Add to devShell buildInputs:
```nix
klaus.packages.${system}.default
```

Then `nix flake lock --update-input klaus` to pick up new versions after pushing changes.

## Workflow: developing klaus while using it elsewhere

Klaus development happens alongside using it on other repos. Ideas come up while using the tool — capture them as issues, fix them later.

**Day-to-day:**
```bash
# In cosmo or build repo, notice a missing feature
gh issue create --repo patflynn/klaus --title "Support --model flag"

# Later, switch to klaus dev
cd ~/hack/klaus
nix develop

# Fix it
make test && make install   # fast local iteration
git push                    # CI runs, other repos can update

# In the other repo, pick up the change
nix flake lock --update-input klaus
```

**Two paths to get the binary:**
- `make install` — fast, uses `go build`, good for active development
- `nix build .` / flake input — reproducible, what CI and other repos use

They're not in conflict. Use `make install` when you're iterating fast. The nix package is the source of truth for distribution.

## Adding a new command

1. Create `internal/cmd/yourcommand.go`
2. Define a `cobra.Command` and register it in `init()` with `rootCmd.AddCommand(...)`
3. Use packages from `internal/` for the logic — keep command files thin
4. `make test && make build` to verify
5. Commit, push, CI validates

## Adding a new internal package

1. Create `internal/yourpkg/yourpkg.go` + `internal/yourpkg/yourpkg_test.go`
2. Write tests first if the logic is pure
3. For packages that shell out (like `git`, `tmux`), test command construction or use temp resources
4. `make test` to verify

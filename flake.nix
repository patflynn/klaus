{
  description = "klaus — Multi-agent orchestrator for Claude Code";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        baseVersion = pkgs.lib.strings.trim (builtins.readFile ./VERSION);
        version = if (self ? rev) then "${baseVersion}-${builtins.substring 0 7 self.rev}" else "${baseVersion}-dirty";

        klaus = pkgs.buildGoModule {
          pname = "klaus";
          inherit version;
          src = ./.;
          vendorHash = "sha256-vh51S12HVVFjHr0DSLZlvBFLWarOlFN7vxXApVb69Dk=";
          ldflags = [ "-X" "github.com/patflynn/klaus/internal/cmd.version=${version}" ];
          nativeBuildInputs = [ pkgs.git ];
        };

        # goCheck runs `script` over klaus's source and the package's vendored
        # modules, with `inputs` on PATH besides go and git, and succeeds with
        # an empty output.
        goCheck = name: { inputs ? [ ], script }: klaus.overrideAttrs (old: {
          pname = "klaus-${name}";
          # The go-modules derivation is named after pname; pin the package's own
          # so every check shares one vendored-dependency derivation.
          inherit (klaus) goModules;
          nativeBuildInputs = old.nativeBuildInputs ++ inputs;
          buildPhase = ''
            runHook preBuild
            # Test file paths stay real, as in buildGoModule's own check phase.
            export GOFLAGS=''${GOFLAGS//-trimpath/}
            ${script}
            runHook postBuild
          '';
          doCheck = false;
          installPhase = "touch $out";
          dontFixup = true;
        });
      in
      {
        packages.default = klaus;

        checks = {
          # The package's check phase runs the unit suite, which checks.unit
          # owns, so this check only builds.
          build = klaus.overrideAttrs { doCheck = false; };

          unit = goCheck "unit" {
            inputs = [ pkgs.tmux ];
            script = "go test ./...";
          };

          vet = goCheck "vet" {
            script = "go vet ./...";
          };

          e2e = goCheck "e2e" {
            inputs = [ pkgs.tmux pkgs.bash ];
            script = ''
              # tmux finds a server through $TMUX or a socket under TMUX_TMPDIR;
              # keep both inside the build's own temp dir.
              unset TMUX TMUX_PANE
              export TMUX_TMPDIR="$TMPDIR/tmux"
              mkdir -p "$TMUX_TMPDIR"
              go test -tags e2e ./e2e/...
            '';
          };
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            golangci-lint
            gh
            git
            tmux
          ];
        };
      }
    );
}

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
      in
      {
        packages.default = let
          baseVersion = pkgs.lib.strings.trim (builtins.readFile ./VERSION);
          version = if (self ? rev) then "${baseVersion}-${builtins.substring 0 7 self.rev}" else "${baseVersion}-dirty";
        in pkgs.buildGoModule {
          pname = "klaus";
          inherit version;
          src = ./.;
          vendorHash = "sha256-vh51S12HVVFjHr0DSLZlvBFLWarOlFN7vxXApVb69Dk=";
          ldflags = [ "-X" "github.com/patflynn/klaus/internal/cmd.version=${version}" ];
          nativeBuildInputs = [ pkgs.git ];
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

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
          version = if (self ? rev) then "0.3.2-${builtins.substring 0 7 self.rev}" else "0.3.2-dirty";
        in pkgs.buildGoModule {
          pname = "klaus";
          inherit version;
          src = ./.;
          vendorHash = "sha256-QEmX66Gurv5iozyzrdA5Re6ZKPl+TXpZF3BFngMNfJY=";
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

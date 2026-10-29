{
  description = "azukari (預かり, kept in custody) — store for the homelab's data, starting with backups";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-26.05-darwin";

    flake-parts = {
      url = "github:hercules-ci/flake-parts";
      inputs.nixpkgs-lib.follows = "nixpkgs";
    };

    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];

      imports = [ inputs.treefmt-nix.flakeModule ];

      perSystem =
        { pkgs, ... }:
        let
          azukari = pkgs.callPackage ./nix/package.nix { };
        in
        {
          packages = {
            inherit azukari;
            default = azukari;
          };

          # The package's check phase runs `go vet` then `go test` (nix/package.nix).
          checks = {
            inherit azukari;
          };

          devShells.default = pkgs.mkShellNoCC {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.buf
              pkgs.protoc-gen-go
            ];
          };

          treefmt.programs = {
            nixfmt.enable = true;
            gofmt.enable = true;
          };
        };
    };
}

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
        { pkgs, lib, ... }:
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

            # The committed gen/ is what `buf generate` produces from proto/.
            proto-generated =
              pkgs.runCommand "azukari-proto-generated"
                {
                  nativeBuildInputs = [
                    pkgs.buf
                    pkgs.protoc-gen-go
                  ];
                  src = lib.fileset.toSource {
                    root = ./.;
                    fileset = lib.fileset.unions [
                      ./buf.yaml
                      ./buf.gen.yaml
                      ./proto
                      ./gen
                    ];
                  };
                }
                ''
                  export HOME=$TMPDIR
                  cp -r $src work && chmod -R u+w work && cd work
                  find gen -name '*.pb.go' -delete
                  buf generate
                  diff -r $src/gen gen || { echo "gen/ is stale: run buf generate"; exit 1; }
                  touch $out
                '';

            # The store and the operator command never link the encryption package (ADR 0002, 0010).
            no-encryption =
              pkgs.runCommand "azukari-no-encryption"
                {
                  nativeBuildInputs = [ pkgs.go ];
                  inherit (azukari) src;
                }
                ''
                  export HOME=$TMPDIR GOCACHE=$TMPDIR/go-cache GOFLAGS=-mod=vendor GOTOOLCHAIN=local CGO_ENABLED=0
                  cp -r $src work && chmod -R u+w work && cd work
                  cp -r ${azukari.goModules} vendor
                  go list -deps ./cmd/azukari-server ./cmd/azukari > deps
                  if grep chacha20poly1305 deps; then
                    echo "azukari-server or azukari imports encryption code"
                    exit 1
                  fi
                  touch $out
                '';
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

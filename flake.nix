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

    # Test-only: the nixosTests run their validators on the store's logs and metrics (ADR 0009, 0010).
    nisshi = {
      url = "git+ssh://git@forgejo.uchi.scorsi.dev:2222/scorsi/nisshi.git";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    masu = {
      url = "git+ssh://git@forgejo.uchi.scorsi.dev:2222/scorsi/masu.git";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    inputs@{ self, flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];

      imports = [ inputs.treefmt-nix.flakeModule ];

      flake.nixosModules = rec {
        azukari-server = import ./nix/module.nix self;
        default = azukari-server;
      };

      perSystem =
        {
          pkgs,
          lib,
          system,
          ...
        }:
        let
          azukari = pkgs.callPackage ./nix/package.nix { };
          # The example module configuration on a Linux host, whatever system evaluates it.
          linux = if lib.hasSuffix "-linux" system then system else "aarch64-linux";
          nixosFor =
            host: extra:
            inputs.nixpkgs.lib.nixosSystem {
              modules = [
                self.nixosModules.azukari-server
                ./nix/example.nix
                extra
                { nixpkgs.hostPlatform = host; }
              ];
            };
          evaluated = (nixosFor linux { }).config;
          unit = evaluated.systemd.services.azukari-server;
          moduleConfig =
            extra: (nixosFor system extra).config.environment.etc."azukari/azukari-server.toml".source;
        in
        {
          packages = {
            inherit azukari;
            default = azukari;
          };

          # The package's check phase runs `go vet` then `go test` (nix/package.nix). `module-eval`
          # evaluates the module on every system, macOS included; on Linux, `module` builds the file
          # (so `azukari-server check` runs on the module's output) and `module-typo` proves an
          # unknown key fails that build, and the nixosTests run (Linux builders).
          checks = {
            inherit azukari;

            # Forces the unit at eval time; the store paths are dropped so nothing is built.
            module-eval =
              let
                sc = unit.serviceConfig;
              in
              assert
                lib.hasSuffix "/bin/azukari-server run /etc/azukari/azukari-server.toml" sc.ExecStart
                && lib.hasSuffix "kill -HUP $MAINPID" sc.ExecReload
                && sc.User == "azukari"
                && sc.Group == "azukari"
                && sc.RuntimeDirectory == "azukari-server"
                && sc.Restart == "on-failure"
                && sc.ReadWritePaths == [ "-/srv/disk-1/azukari/backup" ]
                && sc.LogRateLimitIntervalSec == "30s"
                && sc.LogRateLimitBurst == 30000
                && sc.ProtectSystem == "strict"
                && sc.CapabilityBoundingSet == ""
                && sc.NoNewPrivileges
                && !(sc ? DynamicUser);
              # A bucket's disk may be absent at start (ADR 0004); no firewall opening.
              assert !(lib.hasInfix "RequiresMountsFor" (builtins.toJSON unit.unitConfig));
              assert evaluated.networking.firewall.allowedTCPPorts == [ ];
              assert
                evaluated.users.users.azukari.isSystemUser
                && evaluated.users.users.azukari.group == "azukari"
                && evaluated.users.groups ? azukari-admin;
              pkgs.runCommand "azukari-module-eval" { } ''
                echo ${
                  lib.escapeShellArg (
                    builtins.unsafeDiscardStringContext (
                      builtins.toJSON {
                        unit = unit.serviceConfig;
                        config = evaluated.environment.etc."azukari/azukari-server.toml".source.name;
                      }
                    )
                  )
                } > $out
              '';

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

            # The store and the operator command never link the sealing package nor x/crypto (ADR 0002, 0010).
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
                  if grep -E '^(github\.com/scorsi/azukari/internal/seal(/|$)|golang\.org/x/crypto/)' deps; then
                    echo "azukari-server or azukari imports the sealing package or golang.org/x/crypto"
                    exit 1
                  fi
                  touch $out
                '';
          }
          // lib.optionalAttrs (lib.hasSuffix "-linux" system) {
            module = moduleConfig { };
            module-typo = pkgs.testers.testBuildFailure (moduleConfig {
              services.azukari-server.settings.listne = [ "[::]:8443" ];
            });
            # nixosTests (nix/tests/, ADR 0010).
            rights = import ./nix/tests/rights.nix {
              inherit self pkgs;
              inherit (inputs) nisshi masu;
            };
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

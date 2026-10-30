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
        azukari-backup = import ./nix/backup-module.nix self;
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
          # The example plus a bucket on `/`, which needs no mount unit.
          rootBucket =
            (nixosFor linux {
              services.azukari-server.settings.buckets = [
                {
                  name = "root";
                  path = "/var/lib/azukari/root";
                  mount_point = "/";
                }
              ];
            }).config.systemd.services.azukari-server;
          moduleConfig =
            extra: (nixosFor system extra).config.environment.etc."azukari/azukari-server.toml".source;
          # The same for the node's backup module.
          backupFor =
            host: extra:
            (inputs.nixpkgs.lib.nixosSystem {
              modules = [
                self.nixosModules.azukari-backup
                ./nix/backup-example.nix
                extra
                { nixpkgs.hostPlatform = host; }
              ];
            }).config;
          backup = backupFor linux { };
          backupConfig = extra: (backupFor system extra).environment.etc."azukari/azukari-backup.toml".source;
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
                && sc.Group == "azukari-admin"
                && sc.SupplementaryGroups == [ "azukari" ]
                && sc.RuntimeDirectory == "azukari"
                && sc.Restart == "on-failure"
                && sc.ReadWritePaths == [ "-/srv" ]
                && sc.LogRateLimitIntervalSec == "30s"
                && sc.LogRateLimitBurst == 30000
                && sc.ProtectSystem == "strict"
                && sc.CapabilityBoundingSet == ""
                && sc.NoNewPrivileges
                && !(sc ? DynamicUser);
              # The store starts after its buckets' mounts (escaped unit names, `/` left out) but a
              # disk may be absent at start (ADR 0004): ordering only. No firewall opening.
              assert lib.elem "srv-disk\\x2d1.mount" unit.after && lib.elem "srv-usb.mount" unit.after;
              # Replication's rsync and ssh (ADR 0008).
              assert lib.all (n: lib.any (p: lib.getName p == n) unit.path) [
                "rsync"
                "openssh"
              ];
              assert !(lib.elem "-.mount" rootBucket.after);
              # Disks: the mount points' parent, made at start, so later (un)mounts reach the store.
              # A bucket on `/`: its own path.
              assert
                rootBucket.serviceConfig.ReadWritePaths == [
                  "-/srv"
                  "-/var/lib/azukari/root"
                ];
              # A mount point right under `/` would make `/` writable: one more failed assertion than
              # the sample host (whose others, e.g. no root fs, fail too; messages unforced).
              assert
                let
                  failed = c: lib.length (lib.filter (a: !a.assertion) c.assertions);
                  mnt = nixosFor linux {
                    services.azukari-server.settings.replicas = [
                      {
                        name = "usb";
                        kind = "dir";
                        destination = "/mnt/azukari";
                        mount_point = "/mnt";
                        every = "1h";
                      }
                    ];
                  };
                in
                failed mnt.config == failed evaluated + 1;
              assert
                !(lib.any (u: lib.hasSuffix ".mount" u) (
                  unit.wants ++ unit.requires ++ unit.bindsTo ++ unit.requisite
                ));
              assert !(lib.hasInfix "RequiresMountsFor" (builtins.toJSON unit.unitConfig));
              assert evaluated.networking.firewall.allowedTCPPorts == [ ];
              assert
                evaluated.users.users.azukari.isSystemUser
                && evaluated.users.users.azukari.group == "azukari"
                && evaluated.users.groups ? azukari-admin
                && lib.elem self.packages.${linux}.azukari evaluated.environment.systemPackages;
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

            # The backup units and timers, one per export (ADR 0030), forced at eval time as `module-eval`.
            backup-module-eval =
              let
                units = backup.systemd.services;
                unit = units.azukari-backup-db;
                sc = unit.serviceConfig;
                # The module's timers only: NixOS defines others.
                timers = lib.filterAttrs (n: _: lib.hasPrefix "azukari" n) backup.systemd.timers;
                # Failed assertions, messages unforced (the sample host fails others, e.g. no root fs).
                failed = c: lib.length (lib.filter (a: !a.assertion) c.assertions);
                # A data key in the store fails the evaluation.
                storeKey = backupFor linux {
                  services.azukari-backup.settings.data_key_file = lib.mkForce "${builtins.storeDir}/x-data.key";
                };
                ex = units.azukari-export-db;
                badName = backupFor linux { services.azukari-backup.exports."..".user = "db"; };
                # 15 + 18 characters: one over Linux's 32 for the group name (ADR 0022).
                longName = backupFor linux {
                  services.azukari-backup.exports.${lib.fixedWidthString 18 "a" ""}.user = "db";
                };
                # Writable paths (ADR 0023): a parent of the directory, or none of them.
                writableFor =
                  paths:
                  backupFor linux {
                    services.azukari-backup.exports.app = {
                      user = "app";
                      command = "true";
                      writablePaths = paths;
                    };
                  };
                parent = writableFor [
                  "/var/lib/app"
                  "/persist"
                ];
                # A sibling sharing the directory's prefix is not a parent.
                noParent = writableFor [
                  "/var/lib/app"
                  "/persist/backups/app2"
                ];
                rootExport = backupFor linux {
                  services.azukari-backup.exports.${lib.fixedWidthString 17 "a" ""} = {
                    user = "root";
                    command = "true";
                  };
                };
                # No export, or a table of the file no export runs: refused (ADR 0030).
                noExport = backupFor linux {
                  services.azukari-backup.exports = lib.mkForce { };
                  services.azukari-backup.settings.exports = lib.mkForce { };
                };
                orphan = backupFor linux { services.azukari-backup.settings.exports.extra.paths = [ "/srv/x" ]; };
              in
              assert
                lib.hasSuffix "/bin/azukari-backup run /etc/azukari/azukari-backup.toml db" sc.ExecStart
                && lib.hasSuffix "/bin/azukari-backup run /etc/azukari/azukari-backup.toml www" units.azukari-backup-www.serviceConfig.ExecStart
                && !(sc ? ExecStartPre)
                && sc.Type == "oneshot"
                && sc.User == "azukari-backup"
                && sc.Group == "azukari-backup"
                && sc.LogRateLimitIntervalSec == "30s"
                && sc.LogRateLimitBurst == 30000
                && sc.ProtectSystem == "strict"
                && sc.ProtectHome == true
                && sc.CapabilityBoundingSet == ""
                && sc.AmbientCapabilities == ""
                && sc.NoNewPrivileges
                && sc.MemoryDenyWriteExecute
                && sc.SystemCallFilter == [ "@system-service" ]
                && !(sc ? PrivateUsers)
                && !(sc ? ReadWritePaths)
                && !(sc ? DynamicUser)
                &&
                  units.azukari-backup-www.serviceConfig
                  == sc // { inherit (units.azukari-backup-www.serviceConfig) ExecStart; };
              # Started by its timer only.
              assert unit.wantedBy == [ ] && unit.restartTriggers == [ ];
              # One timer per export, its own `every`; no node-wide unit nor timer (ADR 0030).
              assert
                lib.attrNames timers == [
                  "azukari-backup-db"
                  "azukari-backup-www"
                ]
                && !(units ? azukari-backup)
                && timers.azukari-backup-db.timerConfig.OnCalendar == "*-*-* 00/6:00"
                && timers.azukari-backup-www.timerConfig.OnCalendar == "daily"
                && lib.all (
                  t:
                  t.wantedBy == [ "timers.target" ]
                  && t.timerConfig.Persistent
                  && t.timerConfig.RandomizedDelaySec == "1h"
                ) (lib.attrValues timers);
              assert failed storeKey == failed backup + 1;
              assert
                backup.users.users.azukari-backup.isSystemUser
                && backup.users.users.azukari-backup.group == "azukari-backup"
                && backup.users.groups ? azukari-backup;
              # One operator command: the package, both binaries in one bin/, is installed (ADR 0028).
              assert lib.elem self.packages.${linux}.azukari backup.environment.systemPackages;
              # No root step: the option is gone (ADR 0020).
              assert !(backup.services.azukari-backup ? preCommand);
              # Exports: unit as the service's user, its directory, ordering and paths (ADR 0021, 0030).
              assert
                ex.serviceConfig.Type == "oneshot"
                && ex.serviceConfig.User == "db"
                && !(ex.serviceConfig ? Group)
                && ex.environment.EXPORT_DIR == "/persist/backups/db"
                && ex.serviceConfig.UMask == "0027"
                && ex.serviceConfig.ProtectSystem == "strict"
                && ex.serviceConfig.ReadWritePaths == [ "/persist/backups/db" ]
                && lib.hasInfix "db-dump" ex.script
                && ex.wantedBy == [ ];
              # Each run requires its own export only.
              assert
                unit.requires == [ "azukari-export-db.service" ]
                && lib.elem "azukari-export-db.service" unit.after
                && units.azukari-backup-www.requires == [ "azukari-export-www.service" ];
              assert lib.all (l: lib.elem l backup.systemd.tmpfiles.rules) [
                "d /persist/backups 0711 root root -"
                "d /persist/backups/db 2750 db azukari-export-db -"
                "z /persist/backups/db 2750 db azukari-export-db -"
              ];
              # One group per export, shared by its user and the backup only (ADR 0022).
              assert
                lib.sort lib.lessThan backup.users.groups.azukari-export-db.members == [
                  "azukari-backup"
                  "db"
                ]
                &&
                  rootExport.users.groups.${"azukari-export-" + lib.fixedWidthString 17 "a" ""}.members == [
                    "azukari-backup"
                  ]
                && !(sc ? SupplementaryGroups);
              # `[exports.<name>] paths` of the file: the directory, plus the deployment's paths.
              assert
                backup.services.azukari-backup.settings.exports.db.paths == [ "/persist/backups/db" ]
                &&
                  lib.sort lib.lessThan backup.services.azukari-backup.settings.exports.www.paths == [
                    "/persist/backups/www"
                    "/var/lib/www"
                  ]
                && !(backup.services.azukari-backup.settings ? paths);
              assert failed badName == failed backup + 1;
              assert failed longName == failed backup + 1 && failed rootExport == failed backup;
              assert failed noExport == failed backup + 1 && failed orphan == failed backup + 1;
              assert
                parent.systemd.services.azukari-export-app.serviceConfig.ReadWritePaths == [
                  "/var/lib/app"
                  "/persist"
                ]
                && failed parent == failed backup
                && failed noParent == failed backup + 1;
              pkgs.runCommand "azukari-backup-module-eval" { } ''
                # `azukari backup` finds azukari-backup in its own bin/ (ADR 0028).
                test -x ${azukari}/bin/azukari && test -x ${azukari}/bin/azukari-backup
                echo ${lib.escapeShellArg (builtins.unsafeDiscardStringContext (builtins.toJSON sc))} > $out
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
            backup-module = backupConfig { };
            backup-module-typo = pkgs.testers.testBuildFailure (backupConfig {
              services.azukari-backup.settings.buckte = "backup";
            });
            # nixosTests (nix/tests/, ADR 0010).
            rights = import ./nix/tests/rights.nix {
              inherit self pkgs;
              inherit (inputs) nisshi masu;
            };
            bucket-mount = import ./nix/tests/bucket-mount.nix {
              inherit self pkgs;
              inherit (inputs) nisshi masu;
            };
            svid-reload = import ./nix/tests/svid-reload.nix {
              inherit self pkgs;
              inherit (inputs) nisshi masu;
            };
            backup-restore = import ./nix/tests/backup-restore.nix {
              inherit self pkgs;
              inherit (inputs) nisshi masu;
            };
            retention = import ./nix/tests/retention.nix {
              inherit self pkgs;
              inherit (inputs) nisshi masu;
            };
            replication = import ./nix/tests/replication.nix {
              inherit self pkgs;
              inherit (inputs) nisshi masu;
            };
            restore-from-dir = import ./nix/tests/restore-from-dir.nix {
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
              pkgs.rsync
            ];
          };

          treefmt.programs = {
            nixfmt.enable = true;
            gofmt.enable = true;
          };
        };
    };
}

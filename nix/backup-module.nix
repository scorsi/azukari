# NixOS module for azukari-backup, a node's oneshot run on a timer (milestone M2): the checked file,
# the unit as the static user `azukari-backup` with no capability (ADR 0020), its read-only hardening
# and journald limits (ADR 0006, 0009, 0011). Services export what they want backed up (ADR 0021).
self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.services.azukari-backup;
  toml = pkgs.formats.toml { };
  # By name, so `package` may be any azukari build, whatever its mainProgram.
  exe = lib.getExe' cfg.package "azukari-backup";
  path = "/etc/azukari/azukari-backup.toml";

  # `azukari-backup check` at build time: an invalid file never becomes a system. It reads no key,
  # SVID nor path, so the build sandbox suffices.
  configFile =
    let
      raw = toml.generate "azukari-backup.toml" cfg.settings;
    in
    pkgs.runCommand "azukari-backup.toml" { } ''
      ${exe} check ${raw}
      cp ${raw} $out
    '';

  # One directory per export (ADR 0021) and its group (ADR 0022): the backup reads it, never writes it.
  exportDir = name: "${cfg.exportsDir}/${name}";
  exportGroup = name: "azukari-export-${name}";
  exportUnits = map (name: "azukari-export-${name}.service") (lib.attrNames cfg.exports);

  # Secrets named in the file: a store path is world-readable.
  secrets = lib.filter (f: f != null) [
    (cfg.settings.data_key_file or null)
    (cfg.settings.tls.key_file or null)
  ];
in
{
  options.services.azukari-backup = {
    enable = lib.mkEnableOption "azukari-backup, the node's backup run on a timer";

    package = mkOption {
      type = types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.azukari;
      defaultText = lib.literalExpression "azukari.packages.\${system}.azukari";
      description = "The azukari package.";
    };

    settings = mkOption {
      inherit (toml) type;
      default = { };
      description = ''
        The backup's TOML (ADR 0011): server, bucket, `data_key_file`, paths and `[tls]`. The run is
        the user `azukari-backup` with no capability (ADR 0020), so the deployment must make
        `data_key_file` owned by `azukari-backup`, mode 0400 (the module cannot check a secret's
        owner), give the SVID directory the group `azukari-backup` (hanko ADR-0005), and give
        `paths` directories the group may read; a file the run cannot read fails the run. Paths
        under /home and /root are not visible. None of the key files may be a store path.
      '';
    };

    timer = mkOption {
      type = types.str;
      default = "daily";
      description = "`OnCalendar=` of the backup timer; a missed run starts at boot (`Persistent=`).";
    };

    randomizedDelay = mkOption {
      type = types.str;
      default = "1h";
      description = "`RandomizedDelaySec=` of the timer: spreads the nodes' runs on the store.";
    };

    exportsDir = mkOption {
      type = types.str;
      default = "/persist/backups";
      description = ''
        The base of the exports' directories (ADR 0021), created `0750 root:azukari-backup` when
        missing. An existing base keeps its mode: the group `azukari-backup` must be able to enter it.
      '';
    };

    exports = mkOption {
      default = { };
      description = ''
        Services' exports, the only data a run backs up (ADR 0020, 0021). Each `<name>` gets the
        directory `<exportsDir>/<name>` (see below), the
        oneshot `azukari-export-<name>.service` that runs `command` as `user` with `EXPORT_DIR` set
        to that directory, and the directory in the backup's `paths`. The backup requires and runs
        after every export: a failed export fails the run. Each export has its own group
        `azukari-export-<name>` (ADR 0022), members `user` and `azukari-backup`, which the
        directory carries (mode 2750, owner `user`); `<name>` may hold 17 characters at most.
      '';
      type = types.attrsOf (
        types.submodule (
          { name, ... }:
          {
            options = {
              user = mkOption {
                type = types.str;
                description = "The service's user: the export runs as it and owns the directory.";
              };
              group = mkOption {
                type = types.nullOr types.str;
                default = null;
                description = "The export's `Group=`, when the service needs one; null keeps the user's group.";
              };
              command = mkOption {
                type = types.str;
                description = ''
                  Shell script (`set -e`) writing the export into `$EXPORT_DIR`; it may write only
                  in `writablePaths`. It must replace the previous export, never add next to it: the history lives
                  in azukari. Files are written group-readable (`UMask=0027`) and get the group
                  `azukari-export-<name>` from the setgid directory. A file hard-linked into it keeps
                  its own group: the command gives it the export's group (`chgrp`, then mode 0640),
                  and links only files the service never writes again (ADR 0022).
                '';
              };
              writablePaths = mkOption {
                type = types.listOf (types.strMatching "/.*");
                default = [ (exportDir name) ];
                defaultText = lib.literalExpression ''[ "<exportsDir>/<name>" ]'';
                description = ''
                  The export unit's `ReadWritePaths=` (ADR 0023): absolute paths, one of them the
                  export's directory or one of its parents. An export that hard-links the service's
                  files sets a common parent of the service's data and the directory: a link across
                  two writable paths is two mounts (`EXDEV`). The unit still writes only what `user` may.
                '';
              };
            };
          }
        )
      );
    };
  };

  config = lib.mkIf cfg.enable {
    assertions =
      map (f: {
        assertion = !lib.hasPrefix builtins.storeDir f;
        message = "services.azukari-backup: ${f} is in the world-readable Nix store; point it at a secret file.";
      }) secrets
      # The name lands in a path, a unit name and a tmpfiles line.
      ++ map (name: {
        assertion = builtins.match "[A-Za-z0-9_-]+" name != null;
        message = "services.azukari-backup.exports.${name}: the name may hold only letters, digits, '_' and '-'.";
      }) (lib.attrNames cfg.exports)
      # Linux group names hold 32 characters at most.
      ++ map (name: {
        assertion = lib.stringLength (exportGroup name) <= 32;
        message = "services.azukari-backup.exports.${name}: the group ${exportGroup name} is longer than 32 characters; shorten the name.";
      }) (lib.attrNames cfg.exports)
      # The export must be able to write its own directory (ADR 0023). Lexical: no `..` resolution.
      ++ lib.mapAttrsToList (name: e: {
        assertion = lib.any (
          p: p == exportDir name || lib.hasPrefix (lib.removeSuffix "/" p + "/") (exportDir name)
        ) e.writablePaths;
        message = "services.azukari-backup.exports.${name}.writablePaths: none is ${exportDir name} or one of its parents.";
      }) cfg.exports;

    users.users.azukari-backup = {
      isSystemUser = true;
      group = "azukari-backup";
    };
    # One group per export (ADR 0022). `members` merges with a user the service's own module
    # declares. Static users: membership is all the units need (initgroups), no
    # SupplementaryGroups=. Root needs no group to chgrp.
    users.groups = {
      azukari-backup = { };
    }
    // lib.mapAttrs' (
      name: e:
      lib.nameValuePair (exportGroup name) {
        members = [ "azukari-backup" ] ++ lib.optional (e.user != "root") e.user;
      }
    ) cfg.exports;

    environment.etc."azukari/azukari-backup.toml".source = configFile;

    services.azukari-backup.settings.paths = lib.mkIf (cfg.exports != { }) (
      lib.mapAttrsToList (name: _: exportDir name) cfg.exports
    );

    # `z` also fixes an existing directory. Setgid: exported files get the export's group.
    systemd.tmpfiles.rules =
      lib.optional (cfg.exports != { }) "d ${cfg.exportsDir} 0750 root azukari-backup -"
      ++ lib.concatLists (
        lib.mapAttrsToList (name: e: [
          "d ${exportDir name} 2750 ${e.user} ${exportGroup name} -"
          "z ${exportDir name} 2750 ${e.user} ${exportGroup name} -"
        ]) cfg.exports
      );

    systemd.services =
      lib.mapAttrs' (
        name: e:
        lib.nameValuePair "azukari-export-${name}" {
          description = "azukari export ${name}";
          script = e.command;
          environment.EXPORT_DIR = exportDir name;
          serviceConfig = {
            Type = "oneshot";
            User = e.user;
            Group = lib.mkIf (e.group != null) e.group;
            UMask = "0027"; # Group-readable for azukari-backup, through the export's group.
            # Hardening as the backup run, writable only in its paths (ADR 0023). Not the run's syscall
            # filter nor MemoryDenyWriteExecute: the command is any service's dump tool.
            ProtectSystem = "strict";
            ReadWritePaths = e.writablePaths;
            ProtectHome = true;
            PrivateTmp = true;
            PrivateDevices = true;
            NoNewPrivileges = true;
            ProtectKernelTunables = true;
            ProtectKernelModules = true;
            ProtectKernelLogs = true;
            ProtectControlGroups = true;
            ProtectClock = true;
            RestrictNamespaces = true;
            RestrictRealtime = true;
            RestrictSUIDSGID = true;
            LockPersonality = true;
            SystemCallArchitectures = "native";
          };
        }
      ) cfg.exports
      // {
        azukari-backup = {
          description = "azukari backup run";
          # Started by the timer only: no run at boot nor on a switch. Each start reruns the exports
          # (oneshots without RemainAfterExit); a failed one fails the run (ADR 0021).
          requires = exportUnits;
          wants = [ "network-online.target" ];
          after = [ "network-online.target" ] ++ exportUnits;
          serviceConfig = {
            Type = "oneshot";
            ExecStart = "${exe} run ${path}";
            # Reads only what its group may (ADR 0020). Never PrivateUsers=: owners would read as 65534.
            User = "azukari-backup";
            Group = "azukari-backup";
            # One line per changed or skipped file at most (ADR 0009): nisshi's per-request values.
            LogRateLimitIntervalSec = lib.mkDefault "30s";
            LogRateLimitBurst = lib.mkDefault 30000;

            # Hardening as the store, read-only: the run reads the paths and writes nothing local.
            UMask = "0077";
            LimitCORE = 0; # The data key must not land in core files.
            CoredumpFilter = "0x0";
            ProtectKernelTunables = true;
            ProtectKernelModules = true;
            ProtectKernelLogs = true;
            ProtectControlGroups = true;
            ProtectClock = true;
            RestrictNamespaces = true;
            RestrictRealtime = true;
            RestrictSUIDSGID = true;
            LockPersonality = true;
            SystemCallArchitectures = "native";
            CapabilityBoundingSet = "";
            AmbientCapabilities = "";
            NoNewPrivileges = true;
            ProtectSystem = "strict";
            ProtectHome = true;
            PrivateTmp = true; # The metadata stream's temporary file (os.CreateTemp).
            PrivateDevices = true;
            ProtectProc = "invisible";
            ProcSubset = "pid";
            MemoryDenyWriteExecute = true; # Pure Go (CGO_ENABLED=0) maps nothing W+X.
            # Not `~@resources`: Go's runtime raises RLIMIT_NOFILE (prlimit64) at start.
            SystemCallFilter = [ "@system-service" ];
            RestrictAddressFamilies = [
              "AF_INET"
              "AF_INET6"
              "AF_UNIX"
            ];
          };
        };
      };

    systemd.timers.azukari-backup = {
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnCalendar = cfg.timer;
        Persistent = true;
        RandomizedDelaySec = cfg.randomizedDelay;
      };
    };
  };
}

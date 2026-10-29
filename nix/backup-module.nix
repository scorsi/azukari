# NixOS module for azukari-backup, a node's oneshot run on a timer (milestone M2): the checked file,
# the unit with its read-only hardening and journald limits (ADR 0006, 0009, 0011), an optional
# command before the run (a service's dump).
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
        root, so the data key (a deployment secret, mode 0400 root) and the SVID files of the
        internal CA are read as they are; none of them may be a store path.
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

    preCommand = mkOption {
      type = types.nullOr types.lines;
      default = null;
      example = "pg_dumpall -U postgres > /var/backup/postgres.sql";
      description = ''
        Shell run before the backup in the same unit (`ExecStartPre=`), e.g. a service's dump into a
        backed-up path; its failure fails the run. It runs as root outside the unit's sandbox, so it
        may write its dump.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = map (f: {
      assertion = !lib.hasPrefix builtins.storeDir f;
      message = "services.azukari-backup: ${f} is in the world-readable Nix store; point it at a secret file.";
    }) secrets;

    environment.etc."azukari/azukari-backup.toml".source = configFile;

    systemd.services.azukari-backup = {
      description = "azukari backup run";
      # Started by the timer only: no run at boot nor on a switch.
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      serviceConfig = {
        Type = "oneshot";
        ExecStartPre =
          lib.mkIf (cfg.preCommand != null)
            "+${pkgs.writeShellScript "azukari-backup-pre" ''
              set -euo pipefail
              ${cfg.preCommand}
            ''}";
        ExecStart = "${exe} run ${path}";
        # Root reads every service's data and the key and SVID files (ADR 0006: owners are saved).
        User = "root";
        Group = "root";
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
        # Read any file whatever its owner and mode; nothing else of root.
        CapabilityBoundingSet = "CAP_DAC_READ_SEARCH";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = "read-only"; # Configured paths may live under /home or /root.
        PrivateTmp = true;
        PrivateDevices = true;
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
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

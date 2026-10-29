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
  };

  config = lib.mkIf cfg.enable {
    assertions = map (f: {
      assertion = !lib.hasPrefix builtins.storeDir f;
      message = "services.azukari-backup: ${f} is in the world-readable Nix store; point it at a secret file.";
    }) secrets;

    users.users.azukari-backup = {
      isSystemUser = true;
      group = "azukari-backup";
    };
    users.groups.azukari-backup = { };

    environment.etc."azukari/azukari-backup.toml".source = configFile;

    systemd.services.azukari-backup = {
      description = "azukari backup run";
      # Started by the timer only: no run at boot nor on a switch.
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
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

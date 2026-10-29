# NixOS module for azukari-server, the store (milestone M1): the checked file, the static user
# and groups (ADR 0005), the unit with its hardening and journald limits (ADR 0004, 0009).
self:
{
  config,
  lib,
  pkgs,
  utils,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.services.azukari-server;
  toml = pkgs.formats.toml { };
  # By name, so `package` may be any azukari build, whatever its mainProgram.
  exe = lib.getExe' cfg.package "azukari-server";
  buckets = cfg.settings.buckets or [ ];
  path = "/etc/azukari/azukari-server.toml";
  # The buckets' mount units, `/` (always mounted) left out.
  mounts = lib.unique (
    map (b: "${utils.escapeSystemdPath b.mount_point}.mount") (
      lib.filter (b: b.mount_point != "/") buckets
    )
  );

  # `azukari-server check` at build time: an invalid file never becomes a system. It reads no SVID,
  # mount nor bucket, so the build sandbox suffices.
  configFile =
    let
      raw = toml.generate "azukari-server.toml" cfg.settings;
    in
    pkgs.runCommand "azukari-server.toml" { } ''
      ${exe} check ${raw}
      cp ${raw} $out
    '';

  # A bucket is created only on its own mount (ADR 0004): the store user cannot write a disk's
  # root, so root makes the empty `path` when `mount_point` is mounted and `path` is absent; the
  # store then lays out FORMAT, objects/ and tmp/ and logs `bucket-created`. Never fails the unit.
  makeBucketDirs = pkgs.writeShellScript "azukari-server-bucket-dirs" ''
    umask 022
    ${lib.concatMapStrings (b: ''
      if ${pkgs.util-linux}/bin/mountpoint -q ${lib.escapeShellArg b.mount_point} && [ ! -e ${lib.escapeShellArg b.path} ]; then
        ${pkgs.coreutils}/bin/install -d -o azukari -g azukari -m 0700 ${lib.escapeShellArg b.path}
      fi
    '') buckets}
    exit 0
  '';
in
{
  options.services.azukari-server = {
    enable = lib.mkEnableOption "azukari-server, the store";

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
        The server's TOML (ADR 0004, 0005, 0011). `tls.*` point at the SVID files the internal CA
        writes for this host, readable through the group `azukari`.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    environment.etc."azukari/azukari-server.toml".source = configFile;

    users.users.azukari = {
      isSystemUser = true;
      group = "azukari";
    };
    users.groups.azukari = { };
    # Owner of the local administration socket (ADR 0005); the deployment adds operators.
    users.groups.azukari-admin = { };

    systemd.services.azukari-server = {
      description = "azukari store";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      # After= the bucket mounts: a slow `nofail` mount still finishes before the store opens its
      # buckets. Ordering only: no Wants= (it would mount a `noauto` disk) and no Requires= nor
      # RequiresMountsFor= (a disk that did not mount must not keep the store down, ADR 0004).
      after = [ "network-online.target" ] ++ mounts;
      # A reload re-reads only the SVID files: a configuration change restarts.
      restartTriggers = [ configFile ];
      serviceConfig = {
        ExecStartPre = "+${makeBucketDirs}";
        ExecStart = "${exe} run ${path}";
        # The CA reloads the unit after a renewal: the store re-reads its SVID files (ADR 0005).
        ExecReload = "${pkgs.coreutils}/bin/kill -HUP $MAINPID";
        Restart = "on-failure";
        User = "azukari";
        Group = "azukari";
        RuntimeDirectory = "azukari-server";
        # `-`: a bucket whose disk is absent has no `path`, and must not fail the unit.
        ReadWritePaths = lib.unique (map (b: "-${b.path}") buckets);
        # One line per request: 1000 lines/s sustained before journald drops (ADR 0009).
        LogRateLimitIntervalSec = lib.mkDefault "30s";
        LogRateLimitBurst = lib.mkDefault 30000;

        # Hardening as hanko and genkan.
        UMask = "0077";
        LimitCORE = 0; # The SVID key must not land in core files.
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
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
      };
    };
  };
}

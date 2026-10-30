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
  # Replicas the store writes on this host (ADR 0008), each on its own mount like a bucket.
  dirReplicas = lib.filter (r: r.kind or "" == "dir") (cfg.settings.replicas or [ ]);
  # The buckets' and dir replicas' mount units, `/` (always mounted) left out.
  mounts = lib.unique (
    map (x: "${utils.escapeSystemdPath x.mount_point}.mount") (
      lib.filter (x: x.mount_point != "/") (buckets ++ dirReplicas)
    )
  );

  # The writable area for disks (ADR 0004, 0008): the parent of each `mount_point`, never the
  # mount point nor anything on the disk. systemd bind-mounts every ReadWritePaths entry into the
  # unit's namespace at start; a bind of a directory on the disk pins that filesystem there, so the
  # host's later unmount does not reach the store (it would keep writing to a detached disk, and a
  # dir replica would never be skipped). The parent stays on the host's filesystem: the namespace
  # receives mounts and unmounts below it (slave propagation) both ways, writable as on the host.
  # Root-owned 0755 on the host, so the store still writes only where the disk's owner lets it.
  # A mount point that is itself below another removable mount is not covered.
  onDisk = lib.filter (x: x.mount_point != "/") (buckets ++ dirReplicas);
  mountParents = lib.unique (map (x: dirOf x.mount_point) onDisk);

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
    ${lib.concatMapStrings (d: ''
      ${pkgs.coreutils}/bin/mkdir -p ${lib.escapeShellArg d}
    '') mountParents}
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
    # A mount point right under `/` would make all of `/` writable (mountParents).
    assertions = map (x: {
      assertion = dirOf x.mount_point != "/";
      message = "services.azukari-server: mount_point ${x.mount_point} sits right under /; mount the disk one level deeper (e.g. /srv/disk).";
    }) onDisk;

    environment.etc."azukari/azukari-server.toml".source = configFile;

    users.users.azukari = {
      isSystemUser = true;
      group = "azukari";
    };
    users.groups.azukari = { };
    # Owner of the local administration socket (ADR 0005); the deployment adds operators.
    users.groups.azukari-admin = { };
    # The operator command (ADR 0024).
    environment.systemPackages = [ cfg.package ];

    systemd.services.azukari-server = {
      description = "azukari store";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      # After= the bucket mounts: a slow `nofail` mount still finishes before the store opens its
      # buckets. Ordering only: no Wants= (it would mount a `noauto` disk) and no Requires= nor
      # RequiresMountsFor= (a disk that did not mount must not keep the store down, ADR 0004).
      after = [ "network-online.target" ] ++ mounts;
      # Replication runs rsync, over ssh for kind ssh (ADR 0008).
      path = [
        pkgs.rsync
        pkgs.openssh
      ];
      # A reload re-reads only the SVID files: a configuration change restarts.
      restartTriggers = [ configFile ];
      serviceConfig = {
        ExecStartPre = "+${makeBucketDirs}";
        ExecStart = "${exe} run ${path}";
        # The CA reloads the unit after a renewal: the store re-reads its SVID files (ADR 0005).
        ExecReload = "${pkgs.coreutils}/bin/kill -HUP $MAINPID";
        Restart = "on-failure";
        User = "azukari";
        # The process's group is the administration socket's: $RUNTIME_DIRECTORY/admin.sock,
        # mode 0660, group azukari-admin (ADR 0005, 0024). Its files are 0600/0700 (UMask): the
        # group gives operators nothing else. `azukari` stays for the SVID files the CA writes.
        Group = "azukari-admin";
        SupplementaryGroups = [ "azukari" ];
        RuntimeDirectory = "azukari";
        # The parents of the disks' mount points (see mountParents, made by ExecStartPre); a bucket
        # or dir replica on `/` gets its own `path` or `destination`. `-`: an absent one must not
        # fail the unit. The deployment makes each dir replica's destination, writable by the store.
        ReadWritePaths = lib.unique (
          map (d: "-${d}") mountParents
          ++ map (b: "-${b.path}") (lib.filter (b: b.mount_point == "/") buckets)
          ++ map (r: "-${r.destination}") (lib.filter (r: r.mount_point == "/") dirReplicas)
        );
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

# nixosTest `restore-from-dir` (ADR 0010, milestone M5, ADR 0006, 0008, 0028): node-a backs up a
# tree on the store, whose `dir` replica on a second disk copies the bucket. The store is stopped
# for good; the driver hands the replica directory, as a tar of `<destination>/<bucket>` keeping
# its layout, to a fresh VM that never had a store nor an SVID. There, with only node-a's backup
# file (no [tls], no server running) and its data key, `azukari backup snapshots --from-dir`
# lists the snapshot, `restore --verify --from-dir` passes, `restore --from-dir` as root gives
# back the tree (contents, modes, owners, mtimes, symlinks), and a copy with one byte flipped in
# a chunk fails the restore. The steps are restore-from-dir.nu.
args@{
  self,
  pkgs,
  nisshi,
  ...
}:
let
  t = import ./common.nix args;
  inherit (pkgs) lib;
  system = pkgs.stdenv.hostPlatform.system;
  package = self.packages.${system}.azukari;
  nu = "AZUKARI_BACKUP=${package}/bin/azukari-backup ${t.nu "restore-from-dir.nu"}";

  # node-a's data key (ADR 0006), generated once at build time, on both VMs.
  dataKey = pkgs.runCommand "azukari-test-data-key" { nativeBuildInputs = [ pkgs.openssl ]; } ''
    openssl rand -hex 32 > $out
  '';

  # node-a's backup file; the store's copy adds [tls] (straight from the test CA's files), the
  # fresh VM's has none.
  node = {
    server = "https://127.0.0.1:8443";
    bucket = "backup";
    data_key_file = "${dataKey}";
    exports.tree.paths = [ "/srv/tree-node-a" ];
  };
  backupFile = settings: {
    "azukari-test/node-a.toml".source = (pkgs.formats.toml { }).generate "node-a.toml" settings;
  };

  bucket = builtins.head t.store.services.azukari-server.settings.buckets;
in
pkgs.testers.runNixOSTest {
  name = "azukari-restore-from-dir";

  nodes.store = {
    imports = [ t.store ];
    environment.etc = backupFile (
      node
      // {
        tls = {
          trust_domain = "example.org";
          server_id = t.id "store" "azukari-server";
          svid_file = "${t.ca}/node-a/svid.pem";
          key_file = "${t.ca}/node-a/key.pem";
          bundle_file = "${t.ca}/bundle.pem";
        };
      }
    );
    # The `dir` replica's disk and destination, as in replication.nix.
    virtualisation.emptyDiskImages = [
      {
        size = 256;
        driveConfig.deviceExtraOpts.serial = "disk2";
      }
    ];
    virtualisation.fileSystems."/srv/disk2" = {
      device = "/dev/disk/by-id/virtio-disk2";
      fsType = "ext4";
      autoFormat = true;
      options = [ "nofail" ];
    };
    systemd.services.azukari-replica-dir = {
      wantedBy = [ "srv-disk2.mount" ];
      bindsTo = [ "srv-disk2.mount" ];
      after = [ "srv-disk2.mount" ];
      before = [ "azukari-server.service" ];
      serviceConfig.Type = "oneshot";
      script = "${pkgs.coreutils}/bin/install -d -o azukari -g azukari -m 0700 /srv/disk2/azukari";
    };
    services.azukari-server.settings = {
      classes.standard.replicas = lib.mkForce [ "local" ];
      replicas = [
        {
          name = "local";
          kind = "dir";
          destination = "/srv/disk2/azukari";
          mount_point = "/srv/disk2";
          every = "5s";
        }
      ];
      # Chunks up to 4 MiB sealed (common.nix's 1 MiB refuses them).
      buckets = lib.mkForce [ (bucket // { max_object_bytes = 8388608; }) ];
    };
  };

  # Fresh: the package (`azukari` and `azukari-backup`), node-a's file without [tls] and its data
  # key; no store, no SVID, no tree.
  nodes.restorer = {
    system.stateVersion = "26.05";
    environment.systemPackages = [ package ];
    environment.etc = backupFile node;
  };

  testScript = ''
    start_all()
    store.wait_for_open_port(8443, "127.0.0.1")
    restorer.wait_for_unit("multi-user.target")

    with subtest("node-a backs up its tree, the dir replica copies the bucket"):
        store.succeed("${nu} backup")

    # The store's logs and metrics, while it still runs.
    ${t.validate "store"}

    with subtest("the store stopped, its replica directory handed to the fresh VM"):
        store.succeed("${nu} stop")
        for f in ("replica.tar", "tree.listing", "snapshot"):
            store.copy_from_machine(f"/tmp/{f}")
            restorer.copy_from_host(str(store.out_dir / f), f"/tmp/{f}")
        restorer.succeed("${nu} extract")

    with subtest("snapshots and restore --verify from the copy"):
        restorer.succeed("${nu} snapshots")
        restorer.succeed("${nu} verify")

    with subtest("restore from the copy as root: the tree compares equal"):
        restorer.succeed("${nu} restore")

    with subtest("a byte flipped in a chunk of the copy fails the restore"):
        restorer.succeed("${nu} tampered")

    store.fail("systemctl is-active azukari-server.service")
    restorer.succeed("${nisshi.lib.validateLogs.${system}} validate --logs < /var/log/azukari-cli.log")
  '';
}

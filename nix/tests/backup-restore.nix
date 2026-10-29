# nixosTest `backup-restore` (ADR 0010, v0 acceptance, milestone M2): node-a backs up a varied
# tree exported through `services.azukari-backup.exports` (with a file hard-linked from the
# service's data outside the export, ADR 0022, 0023) and a second export run as the non-root
# user alice, who must traverse the exports base to reach her directory; the run as
# `azukari-backup` with no capability (ADR 0020, 0021); a second, fresh VM holding only node-a's
# data key and SVID restores it as root, and the trees compare equal (`diff -r` and a stat
# listing). One file changes, the second backup adds only a few chunks, the first snapshot still
# restores the original tree; `snapshots` and `restore --verify` run unprivileged; a file the
# group cannot read fails the run (ADR 0005, 0006, 0011).
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
  bin = "${self.packages.${system}.azukari}/bin/azukari-backup";
  dir = "/var/lib/azukari-node";
  nu = "AZUKARI_BACKUP=${bin} ${t.nu "backup-restore.nu"}";

  # node-a's data key (ADR 0006), generated once at build time and given to both node VMs.
  dataKey = pkgs.runCommand "azukari-test-data-key" { nativeBuildInputs = [ pkgs.openssl ]; } ''
    openssl rand -hex 32 > $out
  '';

  # node-a's identity: the data key and its SVID, copied out of the store (the module refuses
  # secrets in the world-readable store) and handed to azukari-backup as ADR 0020 asks (key 0400
  # owned, SVID directory and key through the group), the backup file pointing at the store VM,
  # and the export `data`.
  nodeA = {
    imports = [ self.nixosModules.azukari-backup ];
    system.stateVersion = "26.05";
    systemd.tmpfiles.rules = [
      "d ${dir} 0750 root azukari-backup -"
      "C ${dir}/data.key - - - - ${dataKey}"
      "z ${dir}/data.key 0400 azukari-backup azukari-backup -"
      "C ${dir}/svid.pem - - - - ${t.ca}/node-a/svid.pem"
      "z ${dir}/svid.pem 0644 root root -"
      "C ${dir}/key.pem - - - - ${t.ca}/node-a/key.pem"
      "z ${dir}/key.pem 0640 root azukari-backup -"
      "C ${dir}/bundle.pem - - - - ${t.ca}/bundle.pem"
      "z ${dir}/bundle.pem 0644 root root -"
    ];
    # The export reruns before every run, so the tree must stay the same between runs: the test
    # keeps it in the root-only fixture /srv/fixture and the export copies it. Root, because only
    # root sets the exotic owners (uid 4242, alice) whose restore is tested; a service's export
    # would run as that service's user. `cp -a` keeps modes, owners, mtimes and links.
    # Then a service's sealed file, hard-linked from outside the export (ADR 0022, 0023): the
    # root-only /persist/data holds `day-1`; the export gives it the export's group and links it
    # in, so the backup reads it only through azukari-export-data. The writable path is /persist,
    # the common parent of both on one filesystem: two writable paths are two mounts (EXDEV).
    services.azukari-backup.exports.data = {
      user = "root";
      writablePaths = [ "/persist" ];
      command = ''
        find "$EXPORT_DIR" -mindepth 1 -delete
        cp -a /srv/fixture/. "$EXPORT_DIR"
        chgrp azukari-export-data /persist/data/day-1
        chmod 0640 /persist/data/day-1
        ln /persist/data/day-1 "$EXPORT_DIR/day-1"
      '';
    };
    # The tree holds a setuid file (its mode must survive a restore); the export's
    # RestrictSUIDSGID= would refuse to copy it. Test only: an export of setuid files is no use.
    systemd.services.azukari-export-data.serviceConfig.RestrictSUIDSGID = lib.mkForce false;
    services.azukari-backup = {
      enable = true;
      settings = {
        server = "https://store:8443";
        bucket = "backup";
        data_key_file = "${dir}/data.key";
        tls = {
          trust_domain = "example.org";
          server_id = t.id "store" "azukari-server";
          svid_file = "${dir}/svid.pem";
          key_file = "${dir}/key.pem";
          bundle_file = "${dir}/bundle.pem";
        };
      };
    };
    # The test starts each run itself.
    systemd.timers.azukari-backup.wantedBy = lib.mkForce [ ];
  };
in
pkgs.testers.runNixOSTest {
  name = "azukari-backup-restore";

  nodes = {
    # The common store, reachable from the other VMs, its bucket taking chunks up to 4 MiB sealed.
    store = {
      imports = [ t.store ];
      networking.firewall.allowedTCPPorts = [ 8443 ];
      services.azukari-server.settings = {
        listen = lib.mkForce [ "0.0.0.0:8443" ];
        buckets = lib.mkForce (
          map (b: b // { max_object_bytes = 8388608; }) t.store.services.azukari-server.settings.buckets
        );
      };
    };
    node-a = {
      imports = [ nodeA ];
      # Owners of the tree: a named user and group (numeric uid 4242 has none).
      users.users.alice = {
        uid = 1500;
        isSystemUser = true;
        group = "alice";
      };
      users.groups.alice.gid = 1500;
      # A service's export as its own non-root user: it reaches its directory only through the
      # exports base, which must be traversable (0711). Not on the restorer, which has no alice.
      services.azukari-backup.exports.alice = {
        user = "alice";
        command = ''echo hello-from-alice > "$EXPORT_DIR/hello"'';
      };
    };
    # Fresh: the same key, SVID and file, nothing else of node-a (no tree, no users).
    restorer = nodeA;
  };

  testScript = ''
    def copy(tag):
        """node-a's tree, listing and snapshot `tag` (backup-restore.nu) to the restorer's /tmp."""
        for f in (f"{tag}.tar", f"{tag}.listing", f"{tag}.snapshot"):
            node_a.copy_from_machine(f"/tmp/{f}")
            restorer.copy_from_host(str(node_a.out_dir / f), f"/tmp/{f}")

    start_all()
    store.wait_for_open_port(8443)
    node_a.wait_for_unit("multi-user.target")
    restorer.wait_for_unit("multi-user.target")

    with subtest("the run is azukari-backup with no capability"):
        node_a.succeed("${nu} unit")

    with subtest("node-a's tree: empty files, large random, text, nesting, links, modes, owners, mtimes"):
        node_a.succeed("${nu} tree")

    with subtest("first backup: the export, one snapshot, restored on the fresh VM equal to the tree"):
        node_a.succeed("${nu} first-backup")
        store.succeed("${nu} chunks-first")
        copy("one")
        restorer.succeed("${nu} extract one")
        restorer.succeed("${nu} restore one one")
        restorer.succeed("${nu} alice")

    with subtest("one file changes: the second backup adds only a few chunks"):
        node_a.succeed("${nu} second-backup")
        copy("two")
        restorer.succeed("${nu} extract two")
        store.succeed("${nu} chunks-added")

    with subtest("the first snapshot still restores the original tree, the second the changed one"):
        restorer.succeed("${nu} restore one-again one")
        restorer.succeed("${nu} restore two two")

    with subtest("an unreadable file fails the run"):
        node_a.succeed("${nu} unreadable")

    with subtest("no syscall the run made was denied"):
        node_a.succeed("${nu} syscalls")

    # Node logs (the unit's journal, the CLI's stderr) pass the log schema; then the store's.
    node_a.succeed("journalctl --sync; journalctl _SYSTEMD_UNIT=azukari-backup.service -o cat"
                   " | ${nisshi.lib.validateLogs.${system}} validate --logs")
    for m in (node_a, restorer):
        m.succeed("${nisshi.lib.validateLogs.${system}} validate --logs < /var/log/azukari-cli.log")
    ${t.validate "store"}
  '';
}

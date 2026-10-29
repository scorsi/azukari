# nixosTest `backup-restore` (ADR 0010, v0 acceptance, milestone M2): node-a backs up a varied
# tree exported through `services.azukari-backup.exports`, the run as `azukari-backup` with no
# capability (ADR 0020, 0021); a second, fresh VM holding only node-a's data key and SVID restores
# it as root, and the trees compare equal (`diff -r` and a stat listing). One file changes, the
# second backup adds only a few chunks, the first snapshot still restores the original tree;
# `snapshots` and `restore --verify` run unprivileged; a file the group cannot read fails the run
# (ADR 0005, 0006, 0011).
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
  cfg = "/etc/azukari/azukari-backup.toml";
  dir = "/var/lib/azukari-node";

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
    # Then one hard link given the export's group (ADR 0022): `big` and `linked` share an inode
    # the backup reads only through its membership in azukari-export-data. The link is made inside
    # the export: the sandbox's ReadWritePaths= is its own mount, a link from outside gets EXDEV.
    services.azukari-backup.exports.data = {
      user = "root";
      command = ''
        find "$EXPORT_DIR" -mindepth 1 -delete
        cp -a /srv/fixture/. "$EXPORT_DIR"
        ln "$EXPORT_DIR/big" "$EXPORT_DIR/linked"
        chgrp azukari-export-data "$EXPORT_DIR/linked"
        chmod 0640 "$EXPORT_DIR/linked"
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
    };
    # Fresh: the same key, SVID and file, nothing else of node-a (no tree, no users).
    restorer = nodeA;
  };

  testScript = ''
    cli_log = "/var/log/azukari-cli.log"
    data = "/persist/backups/data"

    def cli(m, args, user=False):
        """azukari-backup on m, as root or (user) as azukari-backup; its stderr (schema v1 lines)
        is kept in /tmp/err and for the validator. The driver runs commands under errexit and pipefail."""
        run = "runuser -u azukari-backup -- " if user else ""
        return m.succeed(
            f"rc=0; {run}${bin} {args} 2>/tmp/err || rc=$?; cat /tmp/err >> {cli_log}; cat /tmp/err >&2; exit $rc"
        )

    def snapshots():
        return sorted(l.split()[0] for l in cli(node_a, "snapshots ${cfg}", user=True).splitlines() if l.strip())

    def listing(m, root):
        """Path, type, mode, numeric owner and group, mtime, link target of every entry."""
        return m.succeed(f"cd {root} && find . -printf '%p %y %m %U %G %T@ %l\\n' | sort")

    def capture(tag):
        """node-a's exported tree now: its listing, and a copy extracted on the restorer for diff -r."""
        node_a.succeed(f"tar -C /persist/backups -cf /tmp/{tag}.tar data")
        node_a.copy_from_machine(f"/tmp/{tag}.tar")
        restorer.copy_from_host(str(node_a.out_dir / f"{tag}.tar"), f"/tmp/{tag}.tar")
        restorer.succeed(f"mkdir -p /orig-{tag} && tar -C /orig-{tag} -xf /tmp/{tag}.tar")
        return listing(node_a, data)

    def restore(snapshot, tag, orig, want):
        """Restore as root into an empty dir on the fresh VM, compare with node-a's tree `orig`,
        then --verify as azukari-backup."""
        target = f"/restore-{tag}"
        restorer.succeed(f"mkdir {target} /verify-{tag}")
        cli(restorer, f"restore ${cfg} {snapshot} {target}")
        restorer.succeed("grep -F '\"event\":\"restore-completed\"' /tmp/err | grep -F '\"verify\":false,\"owners\":true'")
        restorer.succeed(f"diff -r --no-dereference /orig-{orig}/data {target}{data}")
        got = listing(restorer, f"{target}{data}")
        assert got == want, f"stat listings differ:\n{want}\n---\n{got}"
        cli(restorer, f"restore --verify ${cfg} {snapshot} /verify-{tag}", user=True)
        restorer.succeed("grep -F '\"event\":\"restore-completed\"' /tmp/err | grep -F '\"verify\":true,\"owners\":false'")
        restorer.succeed(f"test -z \"$(ls -A /verify-{tag})\"")

    def chunks():
        return int(store.succeed(
            "find /srv/disk1/azukari/backup/objects/node-a/chunks -type f | wc -l").strip())

    def backup():
        # Oneshot: start returns once the export and the run ended, and fails if either failed.
        node_a.succeed("systemctl start azukari-backup.service")

    def unit_events(event):
        return int(node_a.succeed(
            "journalctl _SYSTEMD_UNIT=azukari-backup.service -o cat"
            f" | grep -cF '\"event\":\"{event}\"' || true").strip())

    start_all()
    store.wait_for_open_port(8443)
    node_a.wait_for_unit("multi-user.target")
    restorer.wait_for_unit("multi-user.target")

    with subtest("the run is azukari-backup with no capability"):
        props = dict(l.split("=", 1) for l in node_a.succeed(
            "systemctl show -p User,CapabilityBoundingSet azukari-backup.service").splitlines())
        assert props["User"] == "azukari-backup", props
        assert props["CapabilityBoundingSet"] == "", props

    with subtest("node-a's tree: empty files, large random, text, nesting, links, modes, owners, mtimes"):
        # Group azukari-backup or world-readable everywhere, as an export writes it; the root
        # directory as the module makes the export's (2750 root:azukari-backup).
        node_a.succeed(
            "mkdir -p /srv/fixture/a/b/c /srv/fixture/empty-dir && cd /srv/fixture"
            " && : > empty && : > a/b/c/empty"
            " && head -c 24M /dev/urandom > big"
            " && (yes 'azukari compressible text line' || true) | head -c 3M > text"
            " && echo hello > a/b/hello"
            " && printf '#!/bin/sh\\necho run\\n' > a/run"
            " && ln -s a/b/hello rel-link && ln -s /persist/backups/data/text abs-link"
            " && ln -s missing dangling && ln -s a dir-link"
            " && chgrp -hR azukari-backup ."
            " && chmod 0640 big empty && chmod 4755 a/run"
            " && chmod 0751 a && chmod 1777 empty-dir && chmod 2750 ."
            " && chown -R alice:alice a/b && chown 4242:4243 text"
            " && chown -h 4242:4243 rel-link"
            " && touch -d '2001-02-03 04:05:06.123456789' big text empty a/b/hello"
            " && touch -h -d '2002-03-04 05:06:07.5' rel-link abs-link dangling dir-link"
            " && touch -d '2003-04-05 06:07:08.25' a/b/c a/b a empty-dir ."
        )

    with subtest("first backup: the export, one snapshot, restored on the fresh VM equal to the tree"):
        backup()
        orig = capture("one")
        assert " 4755 " in orig and " 1777 " in orig, orig
        # The hard link, group azukari-export-data and no world read, was backed up and restored.
        node_a.succeed(f"test \"$(stat -c '%G %a %h' {data}/linked)\" = 'azukari-export-data 640 2'")
        assert "./linked f 640 " in orig, orig
        first = snapshots()
        assert len(first) == 1, first
        before = chunks()
        assert before >= 6, f"{before} chunks for a 27 MiB tree (chunks are 4 MiB at most)"
        restore(first[0], "one", "one", orig)

    with subtest("one file changes: the second backup adds only a few chunks"):
        node_a.succeed(
            "printf changed | dd of=/srv/fixture/big bs=1 seek=12582912 conv=notrunc"
            " && touch -d '2004-05-06 07:08:09' /srv/fixture/big"
        )
        backup()
        changed = capture("two")
        both = snapshots()
        assert len(both) == 2 and first[0] in both, both
        added = chunks() - before
        assert 1 <= added <= 4, f"{added} chunks added for one changed file"
        assert unit_events("backup-completed") == 2

    with subtest("the first snapshot still restores the original tree, the second the changed one"):
        restore(first[0], "one-again", "one", orig)
        second = [s for s in both if s != first[0]][0]
        restore(second, "two", "two", changed)

    with subtest("an unreadable file fails the run"):
        # Group azukari-backup from the setgid directory, but no group read: no capability reads it.
        node_a.succeed("echo secret > /srv/fixture/secret && chmod 0600 /srv/fixture/secret")
        node_a.fail("systemctl start azukari-backup.service")
        node_a.succeed(
            "journalctl _SYSTEMD_UNIT=azukari-backup.service -o cat"
            f" | grep -F '\"event\":\"backup-failed\"' | grep -F {data}/secret"
        )
        assert snapshots() == both, "a failed run published a snapshot"
        node_a.succeed("chmod 0640 /srv/fixture/secret")
        backup()
        assert len(snapshots()) == 3
        assert unit_events("backup-completed") == 3

    with subtest("no syscall the run made was denied"):
        # SystemCallFilter= kills with SIGSYS (status 31), logged by systemd and the kernel's audit.
        node_a.succeed("! (journalctl -u azukari-backup.service | grep -E 'status=31/SYS|SIGSYS')")
        node_a.succeed("! (journalctl -k | grep -E 'type=1326.*azukari')")

    # Node logs (the unit's journal, the CLI's stderr) pass the log schema; then the store's.
    node_a.succeed("journalctl --sync; journalctl _SYSTEMD_UNIT=azukari-backup.service -o cat"
                   " | ${nisshi.lib.validateLogs.${system}} validate --logs")
    for m in (node_a, restorer):
        m.succeed(f"${nisshi.lib.validateLogs.${system}} validate --logs < {cli_log}")
    ${t.validate "store"}
  '';
}

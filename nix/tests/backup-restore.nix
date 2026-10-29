# nixosTest `backup-restore` (ADR 0010, v0 acceptance, milestone M2): node-a backs up a varied
# tree through `services.azukari-backup`; a second, fresh VM holding only node-a's data key and
# SVID restores it, and the trees compare equal (`diff -r` and a stat listing). One file changes,
# the second backup adds only a few chunks, the first snapshot still restores the original tree
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
  # secrets in the world-readable store), and the backup file pointing at the store VM.
  nodeA = {
    imports = [ self.nixosModules.azukari-backup ];
    system.stateVersion = "26.05";
    systemd.tmpfiles.rules = [
      "d ${dir} 0700 root root -"
      "C ${dir}/data.key - - - - ${dataKey}"
      "C ${dir}/svid.pem - - - - ${t.ca}/node-a/svid.pem"
      "C ${dir}/key.pem - - - - ${t.ca}/node-a/key.pem"
      "C ${dir}/bundle.pem - - - - ${t.ca}/bundle.pem"
    ];
    services.azukari-backup = {
      enable = true;
      settings = {
        server = "https://store:8443";
        bucket = "backup";
        data_key_file = "${dir}/data.key";
        paths = [ "/srv/data" ];
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

    def cli(m, args):
        """azukari-backup on m; its stderr (schema v1 lines) is kept for the validator.
        The driver runs commands under errexit and pipefail."""
        return m.succeed(
            f"rc=0; ${bin} {args} 2>/tmp/err || rc=$?; cat /tmp/err >> {cli_log}; cat /tmp/err >&2; exit $rc"
        )

    def snapshots():
        return sorted(l.split()[0] for l in cli(node_a, "snapshots ${cfg}").splitlines() if l.strip())

    def listing(m, root):
        """Path, type, mode, numeric owner and group, mtime, link target of every entry."""
        return m.succeed(f"cd {root} && find . -printf '%p %y %m %U %G %T@ %l\\n' | sort")

    def capture(tag):
        """node-a's tree now: its listing, and a copy extracted on the restorer for diff -r."""
        node_a.succeed(f"tar -C /srv -cf /tmp/{tag}.tar data")
        node_a.copy_from_machine(f"/tmp/{tag}.tar")
        restorer.copy_from_host(str(node_a.out_dir / f"{tag}.tar"), f"/tmp/{tag}.tar")
        restorer.succeed(f"mkdir -p /orig-{tag} && tar -C /orig-{tag} -xf /tmp/{tag}.tar")
        return listing(node_a, "/srv/data")

    def restore(snapshot, tag, orig, want):
        """Restore into an empty dir on the fresh VM, compare with node-a's tree `orig`, then --verify."""
        target = f"/restore-{tag}"
        restorer.succeed(f"mkdir {target} /verify-{tag}")
        cli(restorer, f"restore ${cfg} {snapshot} {target}")
        restorer.succeed(f"diff -r --no-dereference /orig-{orig}/data {target}/srv/data")
        got = listing(restorer, f"{target}/srv/data")
        assert got == want, f"stat listings differ:\n{want}\n---\n{got}"
        cli(restorer, f"restore --verify ${cfg} {snapshot} /verify-{tag}")
        restorer.succeed(f"test -z \"$(ls -A /verify-{tag})\"")

    def chunks():
        return int(store.succeed(
            "find /srv/disk1/azukari/backup/objects/node-a/chunks -type f | wc -l").strip())

    def backup():
        # Oneshot: start returns once the run ended, and fails if it failed.
        node_a.succeed("systemctl start azukari-backup.service")

    start_all()
    store.wait_for_open_port(8443)
    node_a.wait_for_unit("multi-user.target")
    restorer.wait_for_unit("multi-user.target")

    with subtest("node-a's tree: empty files, large random, text, nesting, links, modes, owners, mtimes"):
        node_a.succeed(
            "mkdir -p /srv/data/a/b/c /srv/data/empty-dir /srv/data/private && cd /srv/data"
            " && : > empty && : > a/b/c/empty"
            " && head -c 24M /dev/urandom > big"
            " && (yes 'azukari compressible text line' || true) | head -c 3M > text"
            " && echo hello > a/b/hello && echo secret > private/secret"
            " && printf '#!/bin/sh\\necho run\\n' > a/run"
            " && ln -s a/b/hello rel-link && ln -s /srv/data/text abs-link"
            " && ln -s missing dangling && ln -s a dir-link"
            " && chmod 0640 a/b/hello && chmod 0755 a/run && chmod 0600 private/secret"
            " && chmod 0700 private && chmod 0751 a && chmod 1777 empty-dir"
            " && chown -R alice:alice a/b && chown 4242:4243 text private/secret"
            " && chown -h 4242:4243 rel-link"
            " && touch -d '2001-02-03 04:05:06.123456789' big text empty a/b/hello"
            " && touch -h -d '2002-03-04 05:06:07.5' rel-link abs-link dangling dir-link"
            " && touch -d '2003-04-05 06:07:08.25' a/b/c a/b a private empty-dir ."
        )
        orig = capture("one")

    with subtest("first backup: one snapshot, restored on the fresh VM equal to the tree"):
        backup()
        first = snapshots()
        assert len(first) == 1, first
        before = chunks()
        assert before >= 6, f"{before} chunks for a 27 MiB tree (chunks are 4 MiB at most)"
        restore(first[0], "one", "one", orig)

    with subtest("one file changes: the second backup adds only a few chunks"):
        node_a.succeed(
            "printf changed | dd of=/srv/data/big bs=1 seek=12582912 conv=notrunc"
            " && touch -d '2004-05-06 07:08:09' /srv/data/big"
        )
        changed = capture("two")
        backup()
        both = snapshots()
        assert len(both) == 2 and first[0] in both, both
        added = chunks() - before
        assert 1 <= added <= 4, f"{added} chunks added for one changed file"
        node_a.succeed(
            "test $(journalctl _SYSTEMD_UNIT=azukari-backup.service -o cat"
            " | grep -cF '\"event\":\"backup-completed\"') = 2"
        )

    with subtest("the first snapshot still restores the original tree, the second the changed one"):
        restore(first[0], "one-again", "one", orig)
        second = [s for s in both if s != first[0]][0]
        restore(second, "two", "two", changed)

    # Node logs (the unit's journal, the CLI's stderr) pass the log schema; then the store's.
    node_a.succeed("journalctl --sync; journalctl _SYSTEMD_UNIT=azukari-backup.service -o cat"
                   " | ${nisshi.lib.validateLogs.${system}} validate --logs")
    for m in (node_a, restorer):
        m.succeed(f"${nisshi.lib.validateLogs.${system}} validate --logs < {cli_log}")
    ${t.validate "store"}
  '';
}

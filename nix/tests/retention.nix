# nixosTest `retention` (ADR 0010, milestone M3, ADR 0007, 0024), durations of minutes on one VM:
# node-a and node-b back up with the real azukari-backup over loopback; once past `keep`, the
# old snapshots go and the newest of each node stays, by `azukari retention` and once by the
# daily schedule; a bucket left `enabled = false` and `--dry-run` delete nothing; an interrupted
# run's refs and chunks go only after `grace`; a backup running during a prune completes with
# every chunk present; the operator command works for an `azukari-admin` member only, each call
# logged with its uid. The steps are retention.nu.
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
  nu = "AZUKARI_BACKUP=${bin} ${t.nu "retention.nu"}";

  # One data key for both nodes, generated at build time (ADR 0006).
  dataKey = pkgs.runCommand "azukari-test-data-key" { nativeBuildInputs = [ pkgs.openssl ]; } ''
    openssl rand -hex 32 > $out
  '';

  # /etc/azukari-test/<node>-<bucket>.toml: azukari-backup's file for that node and bucket, run
  # as root by retention.nu straight from the test CA's files.
  backupFile = node: bucket: {
    "azukari-test/${node}-${bucket}.toml".source =
      (pkgs.formats.toml { }).generate "${node}-${bucket}.toml"
        {
          server = "https://127.0.0.1:8443";
          inherit bucket;
          data_key_file = "${dataKey}";
          paths = [ "/srv/tree-${node}" ];
          tls = {
            trust_domain = "example.org";
            server_id = t.id "store" "azukari-server";
            svid_file = "${t.ca}/${node}/svid.pem";
            key_file = "${t.ca}/${node}/key.pem";
            bundle_file = "${t.ca}/bundle.pem";
          };
        };
  };

  # Both buckets take chunks up to 4 MiB sealed (common.nix's 1 MiB refuses them).
  backup = builtins.head t.store.services.azukari-server.settings.buckets // {
    max_object_bytes = 8388608;
  };
in
pkgs.testers.runNixOSTest {
  name = "azukari-retention";

  nodes.machine = {
    imports = [ t.store ];
    # retention.nu moves the clock to place the daily run: nothing may move it back.
    services.timesyncd.enable = false;
    users.users.operator = {
      uid = 1600;
      isNormalUser = true;
      extraGroups = [ "azukari-admin" ];
    };
    users.users.stranger = {
      uid = 1601;
      isNormalUser = true;
    };
    environment.etc =
      backupFile "node-a" "backup" // backupFile "node-b" "backup" // backupFile "node-a" "trial";
    # `backup` prunes for real; `trial` keeps the default
    # `enabled = false`. Both run daily at 03:30, which retention.nu places on the clock.
    services.azukari-server.settings.buckets = lib.mkForce [
      (
        backup
        // {
          retention = {
            enabled = true;
            keep = "1m";
            grace = "150s";
            run_at = "03:30";
          };
        }
      )
      (
        backup
        // {
          name = "trial";
          path = "/srv/disk1/azukari/trial";
          nodes = [ (t.id "node-a" "azukari-backup") ];
          retention = {
            keep = "1m";
            run_at = "03:30";
          };
        }
      )
    ];
  };

  testScript = ''
    machine.wait_for_unit("azukari-server.service")
    machine.succeed("${nu} start")
    machine.wait_for_open_port(8443, "127.0.0.1")

    with subtest("the socket is 0660 azukari-admin: a user outside the group cannot open it"):
        machine.succeed("${nu} socket")

    with subtest("first snapshots of node-a, node-b, and node-a in the trial bucket"):
        machine.succeed("${nu} first")

    with subtest("past keep: newer snapshots, and an interrupted run (refs without root)"):
        machine.succeed("${nu} aged")

    with subtest("--dry-run and enabled = false delete nothing and log prune-completed dry_run"):
        machine.succeed("${nu} dry-run")

    with subtest("azukari retention: the old snapshot goes, the newest of each node stays"):
        machine.succeed("${nu} prune")

    with subtest("snapshots and delete-snapshot as an azukari-admin member, admin-call with its uid"):
        machine.succeed("${nu} admin")

    with subtest("past grace, during a backup: the interrupted run goes, the backup restores"):
        machine.succeed("${nu} race")

    with subtest("the daily schedule prunes at run_at"):
        machine.succeed("${nu} schedule")

    machine.succeed("journalctl --sync; journalctl _SYSTEMD_UNIT=race-backup.service -o cat"
                    " | ${nisshi.lib.validateLogs.${system}} validate --logs")
    machine.succeed("${nisshi.lib.validateLogs.${system}} validate --logs < /var/log/azukari-cli.log")
    ${t.validate "machine"}
  '';
}

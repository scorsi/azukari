# nixosTest `exports` (ADR 0010, milestone M6, ADR 0030): one VM, the store and node-a, whose
# module declares two exports on different cadences, `fast` every two minutes and `slow` daily.
# fast runs twice on its own timer, slow once (started by hand: its daily timer would not fire in
# the test); each export lists only its own snapshots and its newest restores alone, equal to its
# source; while no timer fires, the store logs no request from node-a; `azukari retention` with
# a one-minute keep leaves the newest snapshot of each export, slow's older than keep included.
# The steps are exports.nu.
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
  dir = "/var/lib/azukari-node";
  nu = "FIXTURE=${fixture} ${t.nu "exports.nu"}";

  dataKey = pkgs.runCommand "azukari-test-data-key" { nativeBuildInputs = [ pkgs.openssl ]; } ''
    openssl rand -hex 32 > $out
  '';

  # Each export's source, built once: a random file over two chunks and a note, other bytes per
  # export. The commands copy it, so every run exports the same tree.
  fixture = pkgs.runCommand "azukari-test-exports" { } ''
    for e in fast slow; do
      mkdir -p $out/$e
      head -c 6M /dev/urandom > $out/$e/big
      echo "$e" > $out/$e/note
    done
  '';
  export = name: every: {
    user = "root";
    inherit every;
    command = ''
      find "$EXPORT_DIR" -mindepth 1 -delete
      cp -r --no-preserve=mode,ownership ${fixture}/${name}/. "$EXPORT_DIR"
    '';
  };

  backup = builtins.head t.store.services.azukari-server.settings.buckets;
in
pkgs.testers.runNixOSTest {
  name = "azukari-exports";

  nodes.machine = {
    imports = [
      t.store
      self.nixosModules.azukari-backup
    ];
    # exports.nu moves the clock to noon: nothing may move it back.
    services.timesyncd.enable = false;
    # Chunks of 4 MiB sealed (common.nix's 1 MiB refuses them); retention by hand only (run_at
    # is never reached), keep and grace of a minute.
    services.azukari-server.settings.buckets = lib.mkForce [
      (
        backup
        // {
          max_object_bytes = 8388608;
          retention = {
            enabled = true;
            keep = "1m";
            grace = "1m";
            run_at = "03:30";
          };
        }
      )
    ];
    # node-a's key and SVID copied out of the store (ADR 0020), as backup-restore.
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
    services.azukari-backup = {
      enable = true;
      # Each timer fires on its calendar exactly: the test counts fast's runs.
      randomizedDelay = "0";
      exports.fast = export "fast" "*:0/2";
      exports.slow = export "slow" "daily";
      settings = {
        server = "https://127.0.0.1:8443";
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
    # systemd's default AccuracySec= (1 min) would let a two-minute timer slip.
    systemd.timers.azukari-backup-fast.timerConfig.AccuracySec = "1s";
  };

  testScript = ''
    machine.wait_for_unit("azukari-server.service")
    machine.succeed("${nu} start")
    machine.wait_for_open_port(8443, "127.0.0.1")

    with subtest("slow runs once, started by hand; fast twice, on its own timer"):
        machine.succeed("${nu} slow")
        machine.succeed("${nu} fast")

    with subtest("no timer fires: the store logs no request from node-a"):
        machine.succeed("${nu} quiet")

    with subtest("each export lists its own snapshots only, the metric per export"):
        machine.succeed("${nu} lists")

    with subtest("retention keeps the newest snapshot of each export, slow's past keep"):
        machine.succeed("${nu} retention")

    with subtest("each export's newest restores alone, equal to its source"):
        machine.succeed("${nu} restore fast")
        machine.succeed("${nu} restore slow")

    machine.succeed("journalctl --sync; journalctl _SYSTEMD_UNIT=azukari-backup-fast.service"
                    " _SYSTEMD_UNIT=azukari-backup-slow.service -o cat"
                    " | ${nisshi.lib.validateLogs.${system}} validate --logs")
    machine.succeed("${nisshi.lib.validateLogs.${system}} validate --logs < /var/log/azukari-cli.log")
    ${t.validate "machine"}
  '';
}

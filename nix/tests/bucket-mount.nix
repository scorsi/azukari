# nixosTest `bucket-mount` (ADR 0010, milestone M1): a bucket whose disk did not mount is
# unavailable and never created on the root filesystem, the other bucket serves (ADR 0004);
# once its disk is mounted and the store restarted, it is created and serves.
args@{ pkgs, ... }:
let
  t = import ./common.nix args;
  curl = "${pkgs.curl}/bin/curl -sf http://${t.metrics}/metrics";
in
pkgs.testers.runNixOSTest {
  name = "azukari-bucket-mount";

  # The common store (bucket `backup` on disk1) plus bucket `archive` on disk2, whose entry is
  # `noauto`: the host boots without it, as a `nofail` disk that did not show up.
  nodes.machine = {
    imports = [ t.store ];
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
      options = [
        "noauto"
        "nofail"
      ];
    };
    # The empty mount point a real host keeps on its root filesystem.
    systemd.tmpfiles.rules = [ "d /srv/disk2 0755 root root -" ];
    services.azukari-server.settings.buckets = [
      {
        name = "archive";
        type = "backup";
        class = "standard";
        path = "/srv/disk2/azukari/archive";
        mount_point = "/srv/disk2";
        max_object_bytes = 1048576;
        disk_reserve = "1MiB";
        nodes = [ (t.id "node-a" "azukari-backup") ];
        retention = {
          keep = "30d";
          run_at = "03:30";
        };
      }
    ];
  };

  testScript = ''
    def call(bucket, method, key, body=None):
        arg = "" if body is None else f" '{body}'"
        return machine.succeed(f"BUCKET={bucket} ${t.client} node-a {method} {key}{arg}").strip()

    def logged(event, bucket):
        machine.wait_until_succeeds(
            "journalctl _SYSTEMD_UNIT=azukari-server.service -o cat"
            f" | grep -F '\"event\":\"{event}\"' | grep -F '\"bucket\":\"{bucket}\"'"
        )

    def available(bucket, value):
        machine.wait_until_succeeds(
            "${curl} | grep -E '^azukari_bucket_available\\{([^}]*,)?bucket=\""
            + bucket + "\"(,[^}]*)?\\} " + value + "$'"
        )

    machine.wait_for_unit("srv-disk1.mount")
    # disk1 may mount after the store started (nofail, no RequiresMountsFor): open the buckets now.
    machine.succeed("systemctl restart azukari-server")
    machine.wait_for_open_port(8443, "127.0.0.1")
    logged("bucket-created", "backup")

    with subtest("archive's disk is not mounted: 503, nothing on the root filesystem"):
        machine.fail("mountpoint -q /srv/disk2")
        logged("bucket-unavailable", "archive")
        assert call("archive", "PUT", "node-a/chunks/c1", "one") == "503 bucket-unavailable"
        assert call("archive", "GET", "node-a/chunks/c1") == "503 bucket-unavailable"
        machine.succeed("test -d /srv/disk2 && test -z \"$(ls -A /srv/disk2)\"")

    with subtest("the other bucket serves"):
        assert call("backup", "PUT", "node-a/chunks/c1", "one") == "201"
        assert call("backup", "GET", "node-a/chunks/c1") == "200 one"
        available("backup", "1")
        available("archive", "0")

    with subtest("mounted and restarted, archive is created on its disk and serves"):
        machine.succeed("systemctl start srv-disk2.mount")
        machine.succeed("mountpoint -q /srv/disk2")
        machine.succeed("systemctl restart azukari-server")
        machine.wait_for_open_port(8443, "127.0.0.1")
        logged("bucket-created", "archive")
        machine.succeed("test \"$(cat /srv/disk2/azukari/archive/FORMAT)\" = 1")
        assert call("archive", "PUT", "node-a/chunks/c1", "two") == "201"
        assert call("archive", "GET", "node-a/chunks/c1") == "200 two"
        assert call("backup", "GET", "node-a/chunks/c1") == "200 one"
        available("archive", "1")
        available("backup", "1")

    ${t.validate "machine"}
  '';
}

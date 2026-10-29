# nixosTest `bucket-mount` (ADR 0010, milestone M1): a bucket whose disk did not mount is
# unavailable and never created on the root filesystem, the other bucket serves (ADR 0004);
# once its disk is mounted and the store restarted, it is created and serves.
args@{ pkgs, ... }:
let
  t = import ./common.nix args;
  nu = t.nu "bucket-mount.nu";
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
    machine.wait_for_open_port(8443, "127.0.0.1")
    machine.succeed("${nu} ready")

    with subtest("archive's disk is not mounted: 503, nothing on the root filesystem"):
        machine.succeed("${nu} unmounted")

    with subtest("the other bucket serves"):
        machine.succeed("${nu} other-serves")

    with subtest("mounted and restarted, archive is created on its disk and serves"):
        machine.succeed("${nu} mount")
        machine.wait_for_open_port(8443, "127.0.0.1")
        machine.succeed("${nu} mounted")

    ${t.validate "machine"}
  '';
}

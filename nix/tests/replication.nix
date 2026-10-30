# nixosTest `replication` (ADR 0010, milestone M4, ADR 0008, 0009): the store replicates its
# bucket to an SSH target VM (rsync over SSH, the target's host key pinned) and to a `dir` target
# on its own second disk. After backups, and after a real prune, both targets hold the bucket
# byte for byte; with the `dir` target's disk unmounted its runs are skipped and it keeps every
# file, and mounted again under the running store its next run succeeds; with the bucket's disk
# unmounted every run is skipped and neither target loses a file. The steps are replication.nu.
args@{
  self,
  pkgs,
  ...
}:
let
  t = import ./common.nix args;
  inherit (pkgs) lib;
  system = pkgs.stdenv.hostPlatform.system;
  bin = "${self.packages.${system}.azukari}/bin/azukari-backup";
  nu = "AZUKARI_BACKUP=${bin} ${t.nu "replication.nu"}";

  # Test material made at build time: the data key (ADR 0006), the store's SSH key to the
  # target, the target's host key, and the store's known_hosts pinning it under the name `target`.
  keys =
    pkgs.runCommand "azukari-test-replication-keys"
      {
        nativeBuildInputs = [
          pkgs.openssl
          pkgs.openssh
        ];
      }
      ''
        mkdir $out
        openssl rand -hex 32 > $out/data-key
        ssh-keygen -q -t ed25519 -N "" -C azukari-test-store -f $out/store
        ssh-keygen -q -t ed25519 -N "" -C azukari-test-target -f $out/host
        echo "target $(cut -d' ' -f1,2 $out/host.pub)" > $out/known_hosts
      '';

  # /etc/azukari-test/<node>-backup.toml: azukari-backup's file for that node, run as root by
  # replication.nu straight from the test CA's files.
  backupFile = node: {
    "azukari-test/${node}-backup.toml".source = (pkgs.formats.toml { }).generate "${node}-backup.toml" {
      server = "https://127.0.0.1:8443";
      bucket = "backup";
      data_key_file = "${keys}/data-key";
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

  bucket = builtins.head t.store.services.azukari-server.settings.buckets;
in
pkgs.testers.runNixOSTest {
  name = "azukari-replication";

  # The SSH target: sshd with the pinned host key, a dedicated user holding the store's key, its
  # destination directory, rsync for the remote side.
  nodes.target = {
    system.stateVersion = "26.05";
    services.openssh = {
      enable = true;
      hostKeys = [
        {
          path = "/etc/ssh/azukari_test_host_key";
          type = "ed25519";
        }
      ];
    };
    environment.etc = {
      "ssh/azukari_test_host_key" = {
        source = "${keys}/host";
        mode = "0600";
      };
      # Where openssh.authorizedKeys.* would put it; keyFiles would read the key at evaluation.
      "ssh/authorized_keys.d/replica" = {
        source = "${keys}/store.pub";
        mode = "0444";
      };
    };
    users.users.replica.isNormalUser = true;
    systemd.tmpfiles.rules = [ "d /srv/replica 0700 replica users -" ];
    environment.systemPackages = [ pkgs.rsync ];
  };

  nodes.store = {
    imports = [ t.store ];
    environment.etc =
      backupFile "node-a"
      // backupFile "node-b"
      // {
        # ssh refuses a private key others can read: the store path's copy is 0444.
        "azukari-test/replica-key" = {
          source = "${keys}/store";
          mode = "0400";
          user = "azukari";
          group = "azukari";
        };
      };
    # The `dir` target's removable disk, mounted at boot.
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
    # The deployment's part (ADR 0008): the `dir` destination, made writable by the store each
    # time its disk mounts, before the store starts. A oneshot bound to the mount, not tmpfiles,
    # so it never lands on the root filesystem when the disk is absent.
    systemd.services.azukari-replica-dir = {
      wantedBy = [ "srv-disk2.mount" ];
      bindsTo = [ "srv-disk2.mount" ];
      after = [ "srv-disk2.mount" ];
      before = [ "azukari-server.service" ];
      serviceConfig.Type = "oneshot";
      script = "${pkgs.coreutils}/bin/install -d -o azukari -g azukari -m 0700 /srv/disk2/azukari";
    };
    services.azukari-server.settings = {
      classes.standard.replicas = lib.mkForce [
        "offsite"
        "local"
      ];
      replicas = [
        {
          name = "offsite";
          kind = "ssh";
          destination = "replica@target:/srv/replica";
          ssh_key_file = "/etc/azukari-test/replica-key";
          known_hosts_file = "${keys}/known_hosts";
          every = "5s";
        }
        {
          name = "local";
          kind = "dir";
          destination = "/srv/disk2/azukari";
          mount_point = "/srv/disk2";
          every = "5s";
        }
      ];
      # Real prunes: keep and grace of a minute; chunks up to 4 MiB sealed (common.nix's 1 MiB
      # refuses them).
      buckets = lib.mkForce [
        (
          bucket
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
    };
  };

  testScript = ''
    def target_listing(tag):
        """The SSH target's copy of the bucket (replication.nu `listing`) to the store's /tmp/<tag>."""
        target.succeed(f"${nu} listing /srv/replica/backup /tmp/{tag}")
        target.copy_from_machine(f"/tmp/{tag}")
        store.copy_from_host(str(target.out_dir / tag), f"/tmp/{tag}")

    start_all()
    target.wait_for_unit("sshd.service")
    store.wait_for_open_port(8443, "127.0.0.1")

    with subtest("after backups both targets hold the bucket byte for byte, restore --verify passes"):
        store.succeed("${nu} first")
        target_listing("first")
        store.succeed("${nu} same /tmp/first")

    with subtest("a real prune is mirrored on both targets"):
        store.succeed("${nu} prune")
        target_listing("pruned")
        store.succeed("${nu} same /tmp/pruned")

    with subtest("dir target unmounted: skipped, nothing deleted; mounted again, no restart: copied"):
        store.succeed("${nu} dir-unmounted")
        store.succeed("${nu} dir-mounted")
        target_listing("remounted")
        store.succeed("${nu} same /tmp/remounted")

    with subtest("bucket unavailable: every run skipped, nothing deleted on either target"):
        store.succeed("${nu} bucket-down")
        target_listing("bucket-down")
        store.succeed("${nu} unchanged /tmp/remounted /tmp/bucket-down")

    ${t.validate "store"}
  '';
}

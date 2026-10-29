# Sample configuration for the module checks in flake.nix: one backup bucket on its own disk
# (ADR 0004), the SVID files of the internal CA (ADR 0005, 0011). Building the file runs
# `azukari-server check` on it.
{
  system.stateVersion = "26.05";

  services.azukari-server = {
    enable = true;
    settings = {
      listen = [ "[::]:8443" ];
      listen_metrics = "127.0.0.1:9464";
      tls = {
        trust_domain = "example.org";
        svid_file = "/var/lib/hanko/azukari/svid.pem";
        key_file = "/var/lib/hanko/azukari/key.pem";
        bundle_file = "/var/lib/hanko/azukari/bundle.pem";
      };
      classes.standard.replicas = [ ];
      buckets = [
        {
          name = "backup";
          type = "backup";
          class = "standard";
          path = "/srv/disk-1/azukari/backup";
          max_object_bytes = 16777216;
          disk_reserve = "20GiB";
          mount_point = "/srv/disk-1";
          nodes = [ "spiffe://example.org/node-a/azukari-backup" ];
          retention = {
            keep = "30d";
            run_at = "03:30";
          };
        }
      ];
    };
  };
}

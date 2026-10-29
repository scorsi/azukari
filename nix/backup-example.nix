# Sample configuration for the backup module checks in flake.nix: one node's paths, its data key
# and the SVID files of the internal CA (ADR 0006, 0011). Building the file runs
# `azukari-backup check` on it.
{
  system.stateVersion = "26.05";

  services.azukari-backup = {
    enable = true;
    settings = {
      server = "https://store.example.org:8443";
      bucket = "backup";
      data_key_file = "/run/secrets/azukari-data-key";
      paths = [
        "/var/lib/a"
        "/var/backup"
      ];
      tls = {
        trust_domain = "example.org";
        server_id = "spiffe://example.org/store-host/azukari-server";
        svid_file = "/var/lib/hanko/azukari-backup/svid.pem";
        key_file = "/var/lib/hanko/azukari-backup/key.pem";
        bundle_file = "/var/lib/hanko/azukari-backup/bundle.pem";
      };
    };
    preCommand = "echo dump > /var/backup/dump";
  };
}

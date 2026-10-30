# Sample configuration for the backup module checks in flake.nix: one node's two exports on
# different cadences (ADR 0021, 0030), its data key and the SVID files of the internal CA (ADR 0006,
# 0011). Building the file runs `azukari-backup check` on it.
{
  system.stateVersion = "26.05";

  services.azukari-backup = {
    enable = true;
    exports = {
      db = {
        user = "db";
        command = "db-dump > \"$EXPORT_DIR/db.sql\"";
        every = "*-*-* 00/6:00";
      };
      # Daily, the module's default.
      www = {
        user = "www";
        command = "cp -a /srv/www/. \"$EXPORT_DIR\"";
      };
    };
    settings = {
      server = "https://store.example.org:8443";
      bucket = "backup";
      data_key_file = "/run/secrets/azukari-data-key";
      # A deployment path added to an export of the module.
      exports.www.paths = [ "/var/lib/www" ];
      tls = {
        trust_domain = "example.org";
        server_id = "spiffe://example.org/store-host/azukari-server";
        svid_file = "/var/lib/hanko/azukari-backup/svid.pem";
        key_file = "/var/lib/hanko/azukari-backup/key.pem";
        bundle_file = "/var/lib/hanko/azukari-backup/bundle.pem";
      };
    };
  };
}

# nixosTest `svid-reload` (ADR 0010, milestone M1, ADR 0016): the test CA reissues the store's
# SVID in the VM and replaces the files as the internal CA does (staged, renamed, key.pem last),
# with no reload: the next handshake sees the new certificate; a key that does not match keeps
# the previous one (ADR 0005); SIGHUP (the unit's reload) still loads the files. One VM.
args@{ pkgs, ... }:
let
  t = import ./common.nix args;
  # svid-reload.nu's `dir`.
  dir = "/var/lib/azukari-svid";
  nu = t.nu "svid-reload.nu";
in
pkgs.testers.runNixOSTest {
  name = "azukari-svid-reload";

  nodes.machine = {
    imports = [ t.store ];
    # A writable copy of the store's SVID (common.nix reads it from the CA's store path).
    systemd.tmpfiles.rules = [
      "d ${dir} 0755 root root -"
      "C ${dir}/svid.pem - - - - ${t.ca}/store/svid.pem"
      "C ${dir}/key.pem - - - - ${t.ca}/store/key.pem"
    ];
    services.azukari-server.settings.tls = {
      svid_file = pkgs.lib.mkForce "${dir}/svid.pem";
      key_file = pkgs.lib.mkForce "${dir}/key.pem";
    };
  };

  testScript = ''
    machine.wait_for_open_port(8443, "127.0.0.1")
    machine.succeed("${nu} start")

    with subtest("renewed files, no reload: the next handshake sees the new SVID"):
        machine.succeed("${nu} renew")

    with subtest("a key that does not match: svid-reload-failed once, the previous SVID still serves"):
        machine.succeed("${nu} mismatch")

    with subtest("SIGHUP still loads the files, before any handshake"):
        machine.succeed("${nu} sighup")

    ${t.validate "machine"}
  '';
}

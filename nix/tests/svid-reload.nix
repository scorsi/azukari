# nixosTest `svid-reload` (ADR 0010, milestone M1, ADR 0016): the test CA reissues the store's
# SVID in the VM and replaces the files as the internal CA does (staged, renamed, key.pem last),
# with no reload: the next handshake sees the new certificate; a key that does not match keeps
# the previous one (ADR 0005); SIGHUP (the unit's reload) still loads the files. One VM.
args@{ pkgs, ... }:
let
  t = import ./common.nix args;
  dir = "/var/lib/azukari-svid";
  openssl = "${pkgs.openssl}/bin/openssl";
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
    import datetime

    journal = "journalctl _SYSTEMD_UNIT=azukari-server.service -o cat"

    def served():
        """SHA-256 fingerprint of the certificate a new handshake gets from the store."""
        return machine.succeed(
            "${openssl} s_client -connect 127.0.0.1:8443 -tls1_3"
            " -cert ${t.ca}/node-a/svid.pem -key ${t.ca}/node-a/key.pem </dev/null 2>/dev/null"
            " | ${openssl} x509 -noout -fingerprint -sha256"
        ).strip()

    def fingerprint(path):
        return machine.succeed(f"${openssl} x509 -noout -fingerprint -sha256 -in {path}").strip()

    def not_after(path):
        end = machine.succeed(f"${openssl} x509 -noout -enddate -in {path}").strip().split("=", 1)[1]
        return datetime.datetime.strptime(end, "%b %d %H:%M:%S %Y %Z").replace(
            tzinfo=datetime.timezone.utc).timestamp()

    def expiry():
        out = machine.succeed("${pkgs.curl}/bin/curl -fsS http://${t.metrics}/metrics")
        lines = [l for l in out.splitlines() if l.startswith("azukari_svid_expiry_timestamp_seconds ")]
        assert len(lines) == 1, out
        return float(lines[0].split()[1])

    def events(name):
        return int(machine.succeed(f"{journal} | grep -cF '\"event\":\"{name}\"' || true").strip())

    machine.wait_for_open_port(8443, "127.0.0.1")

    old = fingerprint("${dir}/svid.pem")
    assert served() == old, "the store serves the SVID it started with"
    assert expiry() == not_after("${dir}/svid.pem")

    def reissue(serial):
        """Shell issuing a new SVID (the internal CA's profile, common.nix) into the current dir."""
        return (
            "${openssl} genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out key.pem"
            " && ${openssl} req -new -key key.pem -subj /O=azukari-test -out leaf.csr"
            " && printf '%s\\n' basicConstraints=critical,CA:FALSE keyUsage=critical,digitalSignature"
            " extendedKeyUsage=serverAuth,clientAuth"
            " 'subjectAltName=URI:${t.id "store" "azukari-server"}' > leaf.ext"
            " && ${openssl} x509 -req -in leaf.csr -CA ${t.ca}/ca.pem -CAkey ${t.ca}/ca.key"
            f" -set_serial {serial} -days 30 -extfile leaf.ext -out svid.pem"
        )

    def replace(name, src):
        """Write as the internal CA does: staged next to the file, then renamed over it."""
        return f"install -m 0644 {src} ${dir}/.{name}.tmp && mv -f ${dir}/.{name}.tmp ${dir}/{name}"

    with subtest("renewed files, no reload: the next handshake sees the new SVID"):
        machine.succeed(
            "cd $(mktemp -d) && " + reissue(4242)
            + " && " + replace("svid.pem", "svid.pem") + " && " + replace("key.pem", "key.pem")
        )
        new = fingerprint("${dir}/svid.pem")
        assert new != old
        assert "serial=1092" in machine.succeed("${openssl} x509 -noout -serial -in ${dir}/svid.pem")
        before = expiry()
        assert served() == new, "the next handshake gets the renewed SVID"
        machine.wait_until_succeeds(f"{journal} | grep -F '\"event\":\"svid-reloaded\"'")
        assert expiry() == not_after("${dir}/svid.pem") != before, "the expiry gauge follows"
        served()
        assert events("svid-reloaded") == 1, "unchanged files are not reloaded again"

    with subtest("a key that does not match: svid-reload-failed once, the previous SVID still serves"):
        machine.succeed(
            "${openssl} genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out /tmp/other.pem"
            " && " + replace("key.pem", "/tmp/other.pem")
        )
        assert events("svid-reload-failed") == 0
        assert served() == new, "handshakes keep the previous SVID"
        machine.wait_until_succeeds(f"{journal} | grep -F '\"event\":\"svid-reload-failed\"'")
        assert served() == new
        assert events("svid-reload-failed") == 1, "one failure per change of the files"
        assert events("svid-reloaded") == 1
        assert expiry() == not_after("${dir}/svid.pem")

    with subtest("SIGHUP still loads the files, before any handshake"):
        machine.succeed(
            "cd $(mktemp -d) && " + reissue(4343)
            + " && " + replace("svid.pem", "svid.pem") + " && " + replace("key.pem", "key.pem")
        )
        third = fingerprint("${dir}/svid.pem")
        machine.succeed("systemctl reload azukari-server")
        machine.wait_until_succeeds(f"test $({journal} | grep -cF '\"event\":\"svid-reloaded\"') -eq 2")
        assert expiry() == not_after("${dir}/svid.pem")
        assert served() == third

    ${t.validate "machine"}
  '';
}
